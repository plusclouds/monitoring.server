package camera

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func init() { plugin.Register(&Snapshot{}) }

// Image checks.
const (
	checkBlur       = "blur"
	checkBrightness = "brightness"
	checkFrozen     = "frozen"
	checkCovered    = "covered"
	checkMoved      = "moved"
)

var allChecks = []string{checkBlur, checkBrightness, checkFrozen, checkCovered, checkMoved}

// Config of a camera.snapshot check.
type Config struct {
	Vendor            string   `json:"vendor,omitempty" jsonschema:"enum=auto,enum=hikvision,enum=dahua,enum=axis,enum=onvif,enum=url,default=auto,description=Where the snapshot comes from. auto tries Hikvision, Dahua, Axis, then ONVIF and remembers what worked; url uses snapshot_url"`
	SnapshotURL       string   `json:"snapshot_url,omitempty" jsonschema:"maxLength=2000,description=A full URL or a path on the camera, e.g. /cgi-bin/snapshot.cgi"`
	Channel           int      `json:"channel,omitempty" jsonschema:"minimum=1,maximum=256,default=1,description=Channel of a multi-channel camera or recorder"`
	HTTPS             bool     `json:"https,omitempty" jsonschema:"description=Use HTTPS (default HTTP, as most cameras serve)"`
	Port              int      `json:"port,omitempty" jsonschema:"minimum=1,maximum=65535,description=HTTP port when not the default. A port in the device address takes precedence"`
	VerifyCertificate bool     `json:"verify_certificate,omitempty" jsonschema:"description=Verify the camera's TLS certificate (with https)"`
	Checks            []string `json:"checks,omitempty" jsonschema:"description=Image checks to run: blur brightness frozen covered moved. Default: all"`
	DarkBelow         float64  `json:"dark_below,omitempty" jsonschema:"minimum=0,maximum=100,default=10,description=WARNING when mean brightness is below this percent"`
	BrightAbove       float64  `json:"bright_above,omitempty" jsonschema:"minimum=0,maximum=100,default=95,description=WARNING when mean brightness is above this percent (overexposed)"`
	BlurBelow         float64  `json:"blur_below,omitempty" jsonschema:"minimum=1,maximum=100,default=40,description=WARNING when sharpness falls below this percent of the reference picture's"`
	MovedAbove        float64  `json:"moved_above,omitempty" jsonschema:"minimum=1,maximum=100,default=25,description=WARNING when the scene differs from the reference picture by more than this percent"`
	FrozenRuns        int      `json:"frozen_runs,omitempty" jsonschema:"minimum=2,maximum=100,default=3,description=CRITICAL when this many snapshots in a row are the same picture"`
	CoveredBelow      float64  `json:"covered_below,omitempty" jsonschema:"minimum=0,maximum=50,default=4,description=CRITICAL when the picture's contrast is below this (lens covered, sprayed, black frame)"`
	ReferenceID       string   `json:"reference_id,omitempty" jsonschema:"maxLength=100,description=Change this value to learn a new reference picture from the next normal snapshot (after re-aiming the camera)"`
}

var defaults = Config{Vendor: "auto", Channel: 1, DarkBelow: 10, BrightAbove: 95, BlurBelow: 40, MovedAbove: 25,
	FrozenRuns: 3, CoveredBelow: 4}

// Metric slots.
const (
	mBrightness = iota
	mContrast
	mSharpness
	mSharpnessRef
	mSceneChange
	mFrozenRuns
	mWidth
	mHeight
	mBytes
	mFetch
	metricCount
)

// Snapshot implements camera.snapshot.
type Snapshot struct{}

func (*Snapshot) Manifest() plugin.Manifest {
	g := func(name, unit, desc string) plugin.MetricDef {
		return plugin.MetricDef{Name: name, Unit: unit, Description: desc, Kind: "gauge", RetentionClass: plugin.RetentionStandard}
	}
	return plugin.Manifest{
		Type: "camera.snapshot",
		Kind: plugin.KindCheck,
		Description: "Camera picture check from a JPEG snapshot (Hikvision, Dahua, Axis, ONVIF or a URL): blur, too dark or overexposed, " +
			"frozen picture, covered lens, and a camera moved away from its reference picture.",
		ConfigSchema:    plugin.SchemaFor[Config](),
		CredentialTypes: []string{"rtsp", "http_basic"},
		Metrics: []plugin.MetricDef{
			g("brightness_percent", "percent", "Mean brightness"),
			g("contrast", "1", "Standard deviation of brightness, 0 to 255"),
			g("sharpness", "1", "Edge sharpness independent of lighting (Laplacian variance over brightness variance)"),
			g("sharpness_percent_of_reference", "percent", "Sharpness compared with the reference picture"),
			g("scene_change_percent", "percent", "Difference from the reference picture's scene"),
			g("frozen_runs", "1", "Snapshots in a row with the same picture"),
			g("width", "px", "Snapshot width"),
			g("height", "px", "Snapshot height"),
			g("image_bytes", "By", "Snapshot size"),
			g("fetch_ms", "ms", "Time to fetch the snapshot"),
		},
		DefaultInterval: 5 * time.Minute,
		MinInterval:     30 * time.Second,
		PerTargetLimit:  1,
		BillingClass:    plugin.BillingStandard,
		WhoopsyMetric:   "fetch_ms",
	}
}

func (*Snapshot) Validate(raw json.RawMessage) error {
	cfg, err := plugin.DecodeConfig(raw, defaults)
	if err != nil {
		return err
	}
	for _, c := range cfg.Checks {
		if !slices.Contains(allChecks, c) {
			return fmt.Errorf("checks: %q is not one of %v", c, allChecks)
		}
	}
	if cfg.Vendor == "url" && cfg.SnapshotURL == "" {
		return errors.New("vendor url needs snapshot_url")
	}
	if cfg.SnapshotURL != "" && !strings.HasPrefix(cfg.SnapshotURL, "/") {
		u, err := url.Parse(cfg.SnapshotURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("snapshot_url must be a path starting with / or an http(s) URL")
		}
		if u.User != nil {
			return errors.New("snapshot_url must not contain credentials; use the check's credential")
		}
	}
	return nil
}

// state is kept between runs.
type state struct {
	Vendor   string     `json:"vendor,omitempty"` // what auto found
	Ref      *reference `json:"ref,omitempty"`
	Thumb    []byte     `json:"thumb,omitempty"`
	SameRuns int        `json:"same,omitempty"`
}

type reference struct {
	ID        string    `json:"id"`
	Hash      uint64    `json:"hash"`
	Sharpness float64   `json:"sharpness"`
	At        time.Time `json:"at"`
}

func (*Snapshot) Run(ctx context.Context, t plugin.Target) (plugin.Result, error) {
	cfg, err := plugin.DecodeConfig(t.Config, defaults)
	if err != nil {
		return plugin.Result{}, err
	}
	base, err := baseURL(t.Address, cfg)
	if err != nil {
		return plugin.Result{Status: plugin.Unknown, Output: err.Error()}, nil
	}
	var st state
	if b, ok := t.State.Get("camera"); ok {
		_ = json.Unmarshal(b, &st)
	}
	c := newHTTP(t, cfg.VerifyCertificate)
	defer c.close()

	start := time.Now()
	img, source, err := fetch(ctx, c, base, cfg, &st)
	fetchMS := float64(time.Since(start).Microseconds()) / 1000
	r := plugin.Result{Metrics: plugin.NaNs(metricCount)}
	if err != nil {
		r.Status, r.Output = classify(err)
		save(t, st)
		return r, nil
	}
	m, err := analyze(img)
	if err != nil {
		save(t, st)
		return plugin.Errorf(plugin.Unknown, "the snapshot from %s is not a picture: %v", source, err), nil
	}
	r.Metrics[mBrightness], r.Metrics[mContrast], r.Metrics[mSharpness] = m.brightness, m.contrast, m.sharpness*1000
	r.Metrics[mWidth], r.Metrics[mHeight], r.Metrics[mBytes], r.Metrics[mFetch] = float64(m.width), float64(m.height), float64(len(img)), fetchMS

	enabled := func(c string) bool { return len(cfg.Checks) == 0 || slices.Contains(cfg.Checks, c) }
	var crit, warn []string
	covered := m.contrast < cfg.CoveredBelow
	dark := m.brightness < cfg.DarkBelow
	bright := m.brightness > cfg.BrightAbove
	if covered && enabled(checkCovered) {
		crit = append(crit, fmt.Sprintf("covered or blocked: almost no contrast (%.1f)", m.contrast))
	}
	if enabled(checkBrightness) && !covered {
		if dark {
			warn = append(warn, fmt.Sprintf("too dark (%.0f %% brightness)", m.brightness))
		}
		if bright {
			warn = append(warn, fmt.Sprintf("overexposed (%.0f %% brightness)", m.brightness))
		}
	}

	// Frozen: the same picture again and again (a stuck encoder).
	if st.Thumb != nil && meanAbsDiff(st.Thumb, m.thumb) < 0.3 {
		st.SameRuns++
	} else {
		st.SameRuns = 0
	}
	st.Thumb = m.thumb
	r.Metrics[mFrozenRuns] = float64(st.SameRuns + 1)
	if enabled(checkFrozen) && st.SameRuns+1 >= cfg.FrozenRuns {
		crit = append(crit, fmt.Sprintf("frozen: the same picture %d times in a row", st.SameRuns+1))
	}

	// The reference: learned from the first normal picture, again when
	// reference_id changes. Blur and movement are judged against it, and
	// not while the picture is dark (night, infrared) or covered.
	normal := !covered && !dark && !bright
	if (st.Ref == nil || st.Ref.ID != cfg.ReferenceID) && normal {
		st.Ref = &reference{ID: cfg.ReferenceID, Hash: m.hash, Sharpness: m.sharpness, At: time.Now().UTC()}
	}
	note := ""
	if st.Ref != nil && st.Ref.ID == cfg.ReferenceID && normal {
		change := sceneChange(st.Ref.Hash, m.hash)
		r.Metrics[mSceneChange] = change
		if st.Ref.Sharpness > 0 {
			pct := m.sharpness / st.Ref.Sharpness * 100
			r.Metrics[mSharpnessRef] = pct
			if enabled(checkBlur) && pct < cfg.BlurBelow {
				warn = append(warn, fmt.Sprintf("blurred: sharpness %.0f %% of the reference", pct))
			}
		}
		if enabled(checkMoved) && change > cfg.MovedAbove {
			warn = append(warn, fmt.Sprintf("the scene differs %.0f %% from the reference: camera moved or obstructed?", change))
		}
	} else if st.Ref == nil {
		note = "; no reference picture yet (learned from the first normal picture)"
	}
	save(t, st)

	summary := fmt.Sprintf("%dx%d from %s in %.0f ms, brightness %.0f %%, contrast %.0f", m.width, m.height, source, fetchMS,
		m.brightness, m.contrast)
	switch {
	case len(crit) > 0:
		r.Status, r.Output = plugin.Critical, strings.Join(append(crit, warn...), "; ")+" ("+summary+")"
	case len(warn) > 0:
		r.Status, r.Output = plugin.Warning, strings.Join(warn, "; ")+" ("+summary+")"
	default:
		r.Output = "picture OK: " + summary + note
	}
	return r, nil
}

func save(t plugin.Target, st state) {
	if b, err := json.Marshal(st); err == nil {
		_ = t.State.Set("camera", b)
	}
}

// baseURL is scheme://host[:port] of the camera.
func baseURL(address string, cfg Config) (string, error) {
	if address == "" {
		return "", errors.New("the device has no address")
	}
	scheme := "http"
	if cfg.HTTPS {
		scheme = "https"
	}
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(address, "http://"), "https://"), "/")
	if _, _, err := net.SplitHostPort(host); err != nil && cfg.Port != 0 {
		host = net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(cfg.Port))
	}
	return scheme + "://" + host, nil
}

// vendorPaths are the snapshot URLs of the brands we run.
func vendorPath(vendor string, channel int) string {
	switch vendor {
	case "hikvision":
		return fmt.Sprintf("/ISAPI/Streaming/channels/%d01/picture", channel)
	case "dahua":
		return fmt.Sprintf("/cgi-bin/snapshot.cgi?channel=%d", channel)
	case "axis":
		if channel > 1 {
			return fmt.Sprintf("/axis-cgi/jpg/image.cgi?camera=%d", channel)
		}
		return "/axis-cgi/jpg/image.cgi"
	}
	return ""
}

// fetch returns a snapshot and where it came from. auto tries the vendors
// in turn and remembers the first that answers with a picture.
func fetch(ctx context.Context, c *httpClient, base string, cfg Config, st *state) ([]byte, string, error) {
	get := func(u string) ([]byte, error) {
		if strings.HasPrefix(u, "/") {
			u = base + u
		}
		b, ctype, err := c.do(ctx, "GET", u, "", nil)
		if err == nil && ctype != "" && !strings.HasPrefix(ctype, "image/") && !strings.HasPrefix(ctype, "application/octet-stream") {
			return nil, &statusError{url: redact(u), code: 415}
		}
		return b, err
	}
	onvif := func() ([]byte, string, error) {
		u, err := c.onvifSnapshotURI(ctx, base)
		if err != nil {
			return nil, "onvif", err
		}
		b, err := get(u)
		return b, "onvif", err
	}
	vendor := cfg.Vendor
	if cfg.SnapshotURL != "" && (vendor == "auto" || vendor == "url") {
		b, err := get(cfg.SnapshotURL)
		return b, "snapshot_url", err
	}
	switch vendor {
	case "hikvision", "dahua", "axis":
		b, err := get(vendorPath(vendor, cfg.Channel))
		return b, vendor, err
	case "onvif":
		return onvif()
	}
	order := []string{"hikvision", "dahua", "axis", "onvif"}
	if st.Vendor != "" {
		order = append([]string{st.Vendor}, slices.DeleteFunc(order, func(v string) bool { return v == st.Vendor })...)
	}
	var firstErr error
	for _, v := range order {
		var b []byte
		var err error
		if v == "onvif" {
			b, _, err = onvif()
		} else {
			b, err = get(vendorPath(v, cfg.Channel))
		}
		if err == nil {
			st.Vendor = v
			return b, v, nil
		}
		var se *statusError
		if !errors.As(err, &se) {
			return nil, v, err // the camera does not answer: no point trying the others
		}
		if se.code == 401 || se.code == 403 {
			return nil, v, err
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, "auto", fmt.Errorf("no snapshot URL answered with a picture (tried Hikvision, Dahua, Axis, ONVIF); set vendor or snapshot_url: %w", firstErr)
}

// classify turns a fetch error into a status: no answer is CRITICAL, a
// rejected credential or a wrong URL is UNKNOWN.
func classify(err error) (plugin.Status, string) {
	var pe *plugin.PolicyError
	var se *statusError
	var ne net.Error
	var op *net.OpError
	switch {
	case errors.As(err, &pe):
		return plugin.Unknown, pe.Error()
	case errors.As(err, &se) && (se.code == 401 || se.code == 403):
		return plugin.Unknown, "the camera rejected the credential (" + se.Error() + ")"
	case errors.As(err, &se):
		return plugin.Unknown, err.Error()
	case errors.As(err, &ne) && ne.Timeout(), errors.As(err, &op), errors.Is(err, context.DeadlineExceeded):
		return plugin.Critical, "the camera does not answer: " + err.Error()
	}
	return plugin.Unknown, err.Error()
}
