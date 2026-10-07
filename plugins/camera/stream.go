package camera

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/pion/rtp"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func init() { plugin.Register(&Stream{}) }

// StreamConfig of an rtsp.stream check.
type StreamConfig struct {
	Vendor        string  `json:"vendor,omitempty" jsonschema:"enum=auto,enum=hikvision,enum=dahua,enum=axis,enum=path,default=auto,description=Where the stream is. auto tries the Hikvision, Dahua and Axis paths and remembers what worked; path uses path"`
	Path          string  `json:"path,omitempty" jsonschema:"maxLength=1000,description=Stream path on the camera, e.g. /Streaming/Channels/101"`
	Channel       int     `json:"channel,omitempty" jsonschema:"minimum=1,maximum=256,default=1"`
	Substream     bool    `json:"substream,omitempty" jsonschema:"description=Read the camera's second (lower resolution) stream"`
	Port          int     `json:"port,omitempty" jsonschema:"minimum=1,maximum=65535,default=554,description=RTSP port. A port in the device address takes precedence"`
	TLS           bool    `json:"tls,omitempty" jsonschema:"description=RTSPS (RTSP over TLS); the certificate is not verified"`
	Transport     string  `json:"transport,omitempty" jsonschema:"enum=tcp,enum=udp,default=tcp,description=tcp (interleaved) passes firewalls and NAT; udp shows network loss"`
	SampleSeconds int     `json:"sample_seconds,omitempty" jsonschema:"minimum=2,maximum=30,default=5,description=How long the stream is read"`
	MinFPS        float64 `json:"min_fps,omitempty" jsonschema:"minimum=0,maximum=240,description=WARNING below this frame rate. 0 = no check"`
	Width         int     `json:"width,omitempty" jsonschema:"minimum=0,maximum=16384,description=WARNING when the stream's width differs. 0 = no check"`
	Height        int     `json:"height,omitempty" jsonschema:"minimum=0,maximum=16384,description=WARNING when the stream's height differs. 0 = no check"`
	MaxLoss       float64 `json:"max_loss_percent,omitempty" jsonschema:"minimum=0,maximum=100,default=5,description=WARNING above this share of lost RTP packets"`
}

var streamDefaults = StreamConfig{Vendor: "auto", Channel: 1, Port: 554, Transport: "tcp", SampleSeconds: 5, MaxLoss: 5}

// Metric slots of rtsp.stream.
const (
	sFPS = iota
	sBitrate
	sWidth
	sHeight
	sLoss
	sJitter
	sFirstFrame
	streamMetrics
)

// Stream implements rtsp.stream: DESCRIBE, SETUP and PLAY, then the stream
// is read for a few seconds without decoding video, so it costs little.
type Stream struct{}

func (*Stream) Manifest() plugin.Manifest {
	g := func(name, unit, desc string) plugin.MetricDef {
		return plugin.MetricDef{Name: name, Unit: unit, Description: desc, Kind: "gauge", RetentionClass: plugin.RetentionStandard}
	}
	return plugin.Manifest{
		Type: "rtsp.stream",
		Kind: plugin.KindCheck,
		Description: "Camera video stream (RTSP): frame rate, bitrate, resolution, packet loss and jitter over a few seconds, " +
			"without decoding video. CRITICAL when no frames arrive.",
		ConfigSchema:    plugin.SchemaFor[StreamConfig](),
		CredentialTypes: []string{"rtsp"},
		Metrics: []plugin.MetricDef{
			g("fps", "1/s", "Frames per second"),
			g("bitrate_kbps", "kbit/s", "Video bitrate"),
			g("width", "px", "Video width (from the stream's parameter sets)"),
			g("height", "px", "Video height"),
			g("rtp_loss_percent", "percent", "Lost RTP packets"),
			g("jitter_ms", "ms", "RTP interarrival jitter"),
			g("first_frame_ms", "ms", "Time from connecting to the first frame"),
		},
		DefaultInterval: 5 * time.Minute,
		MinInterval:     time.Minute,
		Slow:            true,
		PerTargetLimit:  1,
		BillingClass:    plugin.BillingStandard,
		WhoopsyMetric:   "fps",
	}
}

func (*Stream) Validate(raw json.RawMessage) error {
	cfg, err := plugin.DecodeConfig(raw, streamDefaults)
	if err != nil {
		return err
	}
	if cfg.Vendor == "path" && cfg.Path == "" {
		return errors.New("vendor path needs path")
	}
	if cfg.Path != "" && !strings.HasPrefix(cfg.Path, "/") {
		return errors.New("path must start with /")
	}
	return nil
}

// streamPath is a vendor's RTSP path.
func streamPath(vendor string, channel int, sub bool) string {
	n := 1
	if sub {
		n = 2
	}
	switch vendor {
	case "hikvision":
		return fmt.Sprintf("/Streaming/Channels/%d0%d", channel, n)
	case "dahua":
		return fmt.Sprintf("/cam/realmonitor?channel=%d&subtype=%d", channel, n-1)
	case "axis":
		if sub {
			return "/axis-media/media.amp?resolution=640x360"
		}
		return "/axis-media/media.amp"
	}
	return ""
}

func (*Stream) Run(ctx context.Context, t plugin.Target) (plugin.Result, error) {
	cfg, err := plugin.DecodeConfig(t.Config, streamDefaults)
	if err != nil {
		return plugin.Result{}, err
	}
	host, err := rtspHost(t.Address, cfg.Port)
	if err != nil {
		return plugin.Result{Status: plugin.Unknown, Output: err.Error()}, nil
	}
	var found string
	if b, ok := t.State.Get("rtsp"); ok {
		found = string(b)
	}
	var paths []string
	switch {
	case cfg.Path != "" && (cfg.Vendor == "auto" || cfg.Vendor == "path"):
		paths = []string{cfg.Path}
	case cfg.Vendor != "auto":
		paths = []string{streamPath(cfg.Vendor, cfg.Channel, cfg.Substream)}
	default:
		for _, v := range []string{"hikvision", "dahua", "axis"} {
			paths = append(paths, streamPath(v, cfg.Channel, cfg.Substream))
		}
		if found != "" {
			paths = append([]string{found}, paths...)
		}
	}

	var firstErr error
	for i, p := range paths {
		if i > 0 && p == paths[0] {
			continue
		}
		r, err := read(ctx, t, cfg, host, p)
		var notFound *pathError
		if errors.As(err, &notFound) && len(paths) > 1 {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err != nil {
			st, out := classifyStream(err)
			return plugin.Result{Status: st, Output: out, Metrics: plugin.NaNs(streamMetrics)}, nil
		}
		_ = t.State.Set("rtsp", []byte(p))
		return judge(cfg, r), nil
	}
	return plugin.Errorf(plugin.Unknown, "no stream found (tried the Hikvision, Dahua and Axis paths); set vendor or path: %v", firstErr), nil
}

// pathError is a DESCRIBE that found no stream at a path.
type pathError struct{ msg string }

func (e *pathError) Error() string { return e.msg }

// reading is what a sample window measured.
type reading struct {
	path, codec    string
	frames         int
	bytes          int64
	window         time.Duration
	firstFrame     time.Duration
	width, height  int
	received, lost uint64
	jitter         float64 // RTP clock units
	clockRate      int
}

func read(ctx context.Context, t plugin.Target, cfg StreamConfig, host, path string) (reading, error) {
	r := reading{path: path}
	scheme := "rtsp"
	if cfg.TLS {
		scheme = "rtsps"
	}
	u, err := base.ParseURL(scheme + "://" + host + path)
	if err != nil {
		return r, fmt.Errorf("invalid stream URL: %w", err)
	}
	if cred, ok := t.Credentials["auth"]; ok {
		u.User = url.UserPassword(cred.Fields["username"], cred.Secret["password"].Reveal())
	}
	proto := gortsplib.ProtocolTCP
	if cfg.Transport == "udp" {
		proto = gortsplib.ProtocolUDP
	}
	dialer := t.Network.Dialer(5*time.Second, nil)
	c := gortsplib.Client{
		Scheme: u.Scheme, Host: u.Host, Protocol: &proto,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		DialContext: dialer.DialContext,
		TLSConfig:   &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}, //nolint:gosec // cameras ship self-signed certificates
		UserAgent:   "monitor-rtsp-check",
	}
	start := time.Now()
	if err := c.Start(); err != nil {
		return r, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, c.Close) // a run that times out stops reading
	defer stop()

	desc, res, err := c.Describe(u)
	if err != nil {
		if res != nil && res.StatusCode == base.StatusNotFound {
			return r, &pathError{fmt.Sprintf("%s: not found", path)}
		}
		return r, err
	}
	medi, forma, codec := videoFormat(desc)
	if medi == nil {
		return r, errors.New("the stream has no video")
	}
	r.codec, r.clockRate = codec, forma.ClockRate()
	r.width, r.height = paramSetSize(forma)
	if _, err := c.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		return r, err
	}

	var mu sync.Mutex
	var first time.Time
	decode := auDecoder(forma)
	c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
		au := decode(pkt)
		mu.Lock()
		defer mu.Unlock()
		r.bytes += int64(len(pkt.Payload))
		if au == nil {
			return
		}
		if first.IsZero() {
			first = time.Now()
			r.firstFrame = first.Sub(start)
		}
		r.frames++
		if w, h := auSize(codec, au); w > 0 {
			r.width, r.height = w, h
		}
	})
	if _, err := c.Play(nil); err != nil {
		return r, err
	}
	window := time.Duration(cfg.SampleSeconds) * time.Second
	if dl, ok := ctx.Deadline(); ok && time.Until(dl)-time.Second < window {
		window = max(time.Until(dl)-time.Second, 500*time.Millisecond)
	}
	select {
	case <-time.After(window):
	case <-ctx.Done():
	}
	mu.Lock()
	defer mu.Unlock()
	r.window = window
	if !first.IsZero() {
		r.window = time.Since(first)
	}
	if st := c.Stats(); st != nil {
		r.received, r.lost, r.jitter = st.Session.InboundRTPPackets, st.Session.InboundRTPPacketsLost, st.Session.InboundRTPPacketsJitter
	}
	return r, nil
}

// videoFormat picks the first H.264 or H.265 video format, else any video.
func videoFormat(desc *description.Session) (*description.Media, format.Format, string) {
	var h4 *format.H264
	if m := desc.FindFormat(&h4); m != nil {
		return m, h4, "H.264"
	}
	var h5 *format.H265
	if m := desc.FindFormat(&h5); m != nil {
		return m, h5, "H.265"
	}
	for _, m := range desc.Medias {
		if m.Type == description.MediaTypeVideo && len(m.Formats) > 0 {
			return m, m.Formats[0], fmt.Sprintf("%T", m.Formats[0])[len("*format."):]
		}
	}
	return nil, nil, ""
}

// auDecoder returns a function that turns RTP packets into access units
// (frames), or counts every marked packet as a frame for other codecs.
func auDecoder(f format.Format) func(*rtp.Packet) [][]byte {
	switch f := f.(type) {
	case *format.H264:
		if d, err := f.CreateDecoder(); err == nil {
			return func(p *rtp.Packet) [][]byte {
				au, err := d.Decode(p)
				if err != nil {
					return nil
				}
				return au
			}
		}
	case *format.H265:
		if d, err := f.CreateDecoder(); err == nil {
			return func(p *rtp.Packet) [][]byte {
				au, err := d.Decode(p)
				if err != nil {
					return nil
				}
				return au
			}
		}
	}
	return func(p *rtp.Packet) [][]byte {
		if p.Marker {
			return [][]byte{p.Payload}
		}
		return nil
	}
}

// paramSetSize reads the resolution from the SDP's parameter sets.
func paramSetSize(f format.Format) (int, int) {
	switch f := f.(type) {
	case *format.H264:
		return h264Size(f.SPS)
	case *format.H265:
		return h265Size(f.SPS)
	}
	return 0, 0
}

// auSize reads the resolution from an SPS inside an access unit.
func auSize(codec string, au [][]byte) (int, int) {
	for _, nalu := range au {
		if len(nalu) == 0 {
			continue
		}
		switch codec {
		case "H.264":
			if h264.NALUType(nalu[0]&0x1f) == h264.NALUTypeSPS {
				return h264Size(nalu)
			}
		case "H.265":
			if h265.NALUType((nalu[0]>>1)&0x3f) == h265.NALUType_SPS_NUT {
				return h265Size(nalu)
			}
		}
	}
	return 0, 0
}

func h264Size(sps []byte) (int, int) {
	var s h264.SPS
	if len(sps) == 0 || s.Unmarshal(sps) != nil {
		return 0, 0
	}
	return s.Width(), s.Height()
}

func h265Size(sps []byte) (int, int) {
	var s h265.SPS
	if len(sps) == 0 || s.Unmarshal(sps) != nil {
		return 0, 0
	}
	return s.Width(), s.Height()
}

// judge turns a reading into a result.
func judge(cfg StreamConfig, r reading) plugin.Result {
	res := plugin.Result{Metrics: plugin.NaNs(streamMetrics)}
	secs := r.window.Seconds()
	if r.frames == 0 {
		res.Status = plugin.Critical
		res.Output = fmt.Sprintf("%s stream at %s: no video frames in %.0f s", r.codec, r.path, secs)
		res.Metrics[sFPS] = 0
		return res
	}
	fps := 0.0
	if r.frames > 1 && secs > 0 {
		fps = float64(r.frames-1) / secs
	}
	res.Metrics[sFPS] = fps
	if secs > 0 {
		res.Metrics[sBitrate] = float64(r.bytes) * 8 / secs / 1000
	}
	if r.width > 0 {
		res.Metrics[sWidth], res.Metrics[sHeight] = float64(r.width), float64(r.height)
	}
	loss := 0.0
	if total := r.received + r.lost; total > 0 {
		loss = float64(r.lost) / float64(total) * 100
	}
	res.Metrics[sLoss] = loss
	if r.clockRate > 0 {
		res.Metrics[sJitter] = r.jitter / float64(r.clockRate) * 1000
	}
	res.Metrics[sFirstFrame] = float64(r.firstFrame.Milliseconds())

	var warn []string
	if cfg.MinFPS > 0 && fps < cfg.MinFPS {
		warn = append(warn, fmt.Sprintf("%.1f fps, below %.1f", fps, cfg.MinFPS))
	}
	if (cfg.Width > 0 && r.width != cfg.Width) || (cfg.Height > 0 && r.height != cfg.Height) {
		warn = append(warn, fmt.Sprintf("resolution %dx%d, expected %dx%d", r.width, r.height, cfg.Width, cfg.Height))
	}
	if loss > cfg.MaxLoss {
		warn = append(warn, fmt.Sprintf("%.1f %% of RTP packets lost", loss))
	}
	summary := fmt.Sprintf("%s %dx%d at %.1f fps, %.0f kbit/s, %.1f %% loss (%s)", r.codec, r.width, r.height, fps,
		res.Metrics[sBitrate], loss, r.path)
	if len(warn) > 0 {
		res.Status, res.Output = plugin.Warning, strings.Join(warn, "; ")+": "+summary
	} else {
		res.Output = summary
	}
	return res
}

func rtspHost(address string, port int) (string, error) {
	if address == "" {
		return "", errors.New("the device has no address")
	}
	h := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(address, "rtsp://"), "rtsps://"), "/")
	if _, _, err := net.SplitHostPort(h); err == nil {
		return h, nil
	}
	if port == 0 {
		port = 554
	}
	return net.JoinHostPort(strings.Trim(h, "[]"), strconv.Itoa(port)), nil
}

// classifyStream: no answer is CRITICAL; a rejected login, a refused
// address or a stream without video is UNKNOWN.
func classifyStream(err error) (plugin.Status, string) {
	var pe *plugin.PolicyError
	var ne net.Error
	var op *net.OpError
	s := err.Error()
	switch {
	case errors.As(err, &pe):
		return plugin.Unknown, pe.Error()
	case strings.Contains(s, "401") || strings.Contains(s, "Unauthorized"):
		return plugin.Unknown, "the camera rejected the credential: " + s
	case errors.As(err, &ne) && ne.Timeout(), errors.As(err, &op), errors.Is(err, context.DeadlineExceeded):
		return plugin.Critical, "the camera's RTSP service does not answer: " + s
	}
	return plugin.Unknown, s
}
