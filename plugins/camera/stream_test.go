package camera

import (
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
	"github.com/plusclouds/monitoring.server/pkg/plugin/plugintest"
)

// sps720p is a real H.264 SPS of a 1280x720 stream (from mediacommon's
// tests); the frames after it are not real video, which the check does not
// decode.
var sps720p = []byte{0x67, 0x64, 0x00, 0x1f, 0xac, 0xd9, 0x40, 0x50, 0x05, 0xbb, 0x01, 0x6c, 0x80, 0x00, 0x00, 0x03,
	0x00, 0x80, 0x00, 0x00, 0x1e, 0x07, 0x8c, 0x18, 0xcb}

// fakeRTSP is an RTSP server with one H.264 stream at path, sending fps
// frames a second while live, behind credentials, a query token
// (token set) or nothing (open set).
type fakeRTSP struct {
	token  string
	open   bool
	server *gortsplib.Server
	stream *gortsplib.ServerStream
	path   string
	addr   string
	mu     sync.Mutex
	live   bool
	done   chan struct{}
}

// authorized checks a request's credentials or token.
func (f *fakeRTSP) authorized(conn *gortsplib.ServerConn, req *base.Request, query string) bool {
	switch {
	case f.open:
		return true
	case f.token != "":
		v, _ := url.ParseQuery(query)
		return v.Get("token") == f.token
	}
	return conn.VerifyCredentials(req, "admin", "cam-secret")
}

func (f *fakeRTSP) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if !f.authorized(ctx.Conn, ctx.Request, ctx.Query) {
		return &base.Response{StatusCode: base.StatusUnauthorized}, nil, liberrors.ErrServerAuth{}
	}
	if f.token != "" {
		if ctx.Path != f.path {
			return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
		}
	} else if ctx.Path+queryOf(ctx.Query) != f.path {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, f.stream, nil
}

func queryOf(q string) string {
	if q == "" {
		return ""
	}
	return "?" + q
}

func (f *fakeRTSP) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	if !f.authorized(ctx.Conn, ctx.Request, ctx.Query) {
		return &base.Response{StatusCode: base.StatusUnauthorized}, nil, liberrors.ErrServerAuth{}
	}
	return &base.Response{StatusCode: base.StatusOK}, f.stream, nil
}

func (f *fakeRTSP) OnPlay(_ *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func newRTSP(t *testing.T, path string, fps int) *fakeRTSP {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	f := &fakeRTSP{path: path, addr: addr, live: true, done: make(chan struct{})}
	f.server = &gortsplib.Server{Handler: f, RTSPAddress: addr}
	if err := f.server.Start(); err != nil {
		t.Fatal(err)
	}
	h264 := &format.H264{PayloadTyp: 96, PacketizationMode: 1, SPS: sps720p, PPS: []byte{0x68, 0xee, 0x3c, 0x80}}
	media := &description.Media{Type: description.MediaTypeVideo, Formats: []format.Format{h264}}
	f.stream = &gortsplib.ServerStream{Server: f.server, Desc: &description.Session{Medias: []*description.Media{media}}}
	if err := f.stream.Initialize(); err != nil {
		t.Fatal(err)
	}
	enc, err := h264.CreateEncoder()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		tick := time.NewTicker(time.Second / time.Duration(fps))
		defer tick.Stop()
		for n := 0; ; n++ {
			select {
			case <-f.done:
				return
			case <-tick.C:
			}
			f.mu.Lock()
			live := f.live
			f.mu.Unlock()
			if !live {
				continue
			}
			au := [][]byte{{0x41, 0x9a, 0x00, 0x01, 0x02, 0x03}}
			if n%fps == 0 {
				au = [][]byte{sps720p, {0x68, 0xee, 0x3c, 0x80}, append([]byte{0x65, 0x88}, make([]byte, 2000)...)}
			}
			pkts, err := enc.Encode(au)
			if err != nil {
				return
			}
			for _, p := range pkts {
				p.Timestamp = uint32(n * 90000 / fps) //nolint:gosec // test timestamps
				_ = f.stream.WritePacketRTP(media, p)
			}
		}
	}()
	t.Cleanup(func() {
		close(f.done)
		f.stream.Close()
		f.server.Close()
	})
	return f
}

func (f *fakeRTSP) setLive(v bool) {
	f.mu.Lock()
	f.live = v
	f.mu.Unlock()
}

func runStream(t *testing.T, f *fakeRTSP, pass string, cfg any, st plugin.StateStore) plugin.Result {
	t.Helper()
	return plugintest.Run(t, &Stream{}, plugintest.Options{Address: f.addr, Config: cfg, Credentials: creds(pass),
		State: st, Timeout: 10 * time.Second})
}

func TestStream(t *testing.T) {
	f := newRTSP(t, "/Streaming/Channels/101", 25)
	st := &plugin.MemState{}
	r := runStream(t, f, "cam-secret", StreamConfig{SampleSeconds: 2}, st)
	if r.Status != plugin.OK || r.Metrics[sWidth] != 1280 || r.Metrics[sHeight] != 720 || r.Metrics[sFPS] < 20 ||
		r.Metrics[sFPS] > 30 || r.Metrics[sBitrate] <= 0 || r.Metrics[sLoss] != 0 || !strings.Contains(r.Output, "H.264 1280x720") {
		t.Fatalf("stream: %v %q %v", r.Status, r.Output, r.Metrics)
	}
	if b, _ := st.Get("rtsp"); string(b) != "/Streaming/Channels/101" {
		t.Errorf("found path not kept: %q", b)
	}
	// Expectations not met: WARNING.
	r = runStream(t, f, "cam-secret", StreamConfig{SampleSeconds: 2, MinFPS: 50, Width: 1920, Height: 1080}, nil)
	if r.Status != plugin.Warning || !strings.Contains(r.Output, "below 50.0") || !strings.Contains(r.Output, "expected 1920x1080") {
		t.Errorf("expectations: %v %q", r.Status, r.Output)
	}
	// The stream stalls: CRITICAL.
	f.setLive(false)
	r = runStream(t, f, "cam-secret", StreamConfig{SampleSeconds: 2}, nil)
	if r.Status != plugin.Critical || !strings.Contains(r.Output, "no video frames") || r.Metrics[sFPS] != 0 {
		t.Errorf("stalled: %v %q", r.Status, r.Output)
	}
}

func TestStreamFailures(t *testing.T) {
	f := newRTSP(t, "/cam/realmonitor?channel=1&subtype=0", 10)
	if r := runStream(t, f, "cam-secret", StreamConfig{SampleSeconds: 2}, nil); r.Status != plugin.OK || !strings.Contains(r.Output, "realmonitor") {
		t.Errorf("dahua path found by auto: %v %q", r.Status, r.Output)
	}
	if r := runStream(t, f, "wrong", StreamConfig{SampleSeconds: 2, Vendor: "dahua"}, nil); r.Status != plugin.Unknown ||
		!strings.Contains(r.Output, "rejected the credential") {
		t.Errorf("wrong password: %v %q", r.Status, r.Output)
	}
	if r := runStream(t, f, "cam-secret", StreamConfig{Vendor: "path", Path: "/nope"}, nil); r.Status != plugin.Unknown {
		t.Errorf("unknown path: %v %q", r.Status, r.Output)
	}
	_, port, _ := net.SplitHostPort(f.addr)
	if r := plugintest.Run(t, &Stream{}, plugintest.Options{Address: "127.0.0.1", Config: StreamConfig{Port: mustAtoi(port)},
		Credentials: creds("cam-secret"), Network: &plugin.NetPolicy{DenyPrivate: true}}); r.Status != plugin.Unknown ||
		!strings.Contains(r.Output, "private or local") {
		t.Errorf("policy: %v %q", r.Status, r.Output)
	}
	if r := plugintest.Run(t, &Stream{}, plugintest.Options{Address: "127.0.0.1:1", Credentials: creds("x"), Timeout: 5 * time.Second,
		Config: StreamConfig{Vendor: "hikvision"}}); r.Status != plugin.Critical {
		t.Errorf("unreachable: %v %q", r.Status, r.Output)
	}
	if err := (&Stream{}).Validate([]byte(`{"vendor":"path"}`)); err == nil {
		t.Error("vendor path without path accepted")
	}
}

func mustAtoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic(err)
	}
	return n
}

// TestStreamURL: a full url with a query token (rtsp://host/live?token=...),
// a url_token credential, and an open stream without any credential.
func TestStreamURL(t *testing.T) {
	const tok = "e681d467aa0ea70df01eb72a6abf95af"
	f := newRTSP(t, "/live/retail", 25)
	f.token = tok
	st := &plugin.MemState{}
	run := func(cfg StreamConfig, cr map[string]plugin.Credential) plugin.Result {
		return plugintest.Run(t, &Stream{}, plugintest.Options{Address: "ignored.invalid", Config: cfg, Credentials: cr,
			State: st, Timeout: 10 * time.Second})
	}
	u := "rtsp://" + f.addr + "/live/retail"
	r := run(StreamConfig{URL: u + "?token=" + tok, SampleSeconds: 2}, nil)
	if r.Status != plugin.OK || r.Metrics[sWidth] != 1280 || strings.Contains(r.Output, tok) || !strings.Contains(r.Output, "/live/retail?…") {
		t.Fatalf("token in url: %v %q", r.Status, r.Output)
	}
	if _, ok := st.Get("rtsp"); ok {
		t.Error("a configured url was remembered in the check state")
	}
	tokenCred := map[string]plugin.Credential{"auth": {Type: "url_token", Secret: map[string]plugin.Secret{"token": plugin.Secret(tok)}}}
	if r := run(StreamConfig{Vendor: "url", URL: u, SampleSeconds: 2}, tokenCred); r.Status != plugin.OK || strings.Contains(r.Output, tok) {
		t.Errorf("url_token credential: %v %q", r.Status, r.Output)
	}
	// The credential replaces a stale token in the url.
	if r := run(StreamConfig{URL: u + "?token=old", SampleSeconds: 2}, tokenCred); r.Status != plugin.OK {
		t.Errorf("credential over url token: %v %q", r.Status, r.Output)
	}
	if r := run(StreamConfig{URL: u, SampleSeconds: 2}, nil); r.Status != plugin.Unknown || !strings.Contains(r.Output, "requires authorization") {
		t.Errorf("no token: %v %q", r.Status, r.Output)
	}
	bad := map[string]plugin.Credential{"auth": {Type: "url_token", Secret: map[string]plugin.Secret{"token": "wrong-token"}}}
	if r := run(StreamConfig{URL: u, SampleSeconds: 2}, bad); r.Status != plugin.Unknown ||
		!strings.Contains(r.Output, "rejected the credential") || strings.Contains(r.Output, "wrong-token") {
		t.Errorf("wrong token: %v %q", r.Status, r.Output)
	}

	open := newRTSP(t, "/open", 25)
	open.open = true
	if r := run(StreamConfig{URL: "rtsp://" + open.addr + "/open", SampleSeconds: 2}, nil); r.Status != plugin.OK {
		t.Errorf("open stream by url: %v %q", r.Status, r.Output)
	}
	if r := plugintest.Run(t, &Stream{}, plugintest.Options{Address: open.addr, Config: StreamConfig{Vendor: "path", Path: "/open", SampleSeconds: 2},
		Timeout: 10 * time.Second}); r.Status != plugin.OK {
		t.Errorf("open stream without credential: %v %q", r.Status, r.Output)
	}

	for _, raw := range []string{`{"vendor":"url"}`, `{"url":"http://cam/live"}`, `{"url":"rtsp:///live"}`,
		`{"url":"rtsp://u:p@cam/live"}`, `{"url":"rtsp://cam/live","path":"/live"}`} {
		if err := (&Stream{}).Validate([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	if err := (&Stream{}).Validate([]byte(`{"url":"rtsps://cam:8322/live/retail?token=x"}`)); err != nil {
		t.Errorf("rejected a valid url: %v", err)
	}
}
