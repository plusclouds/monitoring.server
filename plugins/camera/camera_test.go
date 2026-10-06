package camera

import (
	"bytes"
	"crypto/md5" //nolint:gosec // the fake camera checks digest auth like real ones
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
	"github.com/plusclouds/monitoring.server/pkg/plugin/plugintest"
)

// scene draws a picture: rectangles of varied brightness on a gradient,
// with noise, seeded so the same seed is the same scene.
func scene(seed uint64, w, h int) *image.Gray {
	r := rand.New(rand.NewPCG(seed, seed*7+1)) //nolint:gosec // test pictures, not secrets
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetGray(x, y, color.Gray{Y: uint8(60 + 100*x/w)}) //nolint:gosec // 60..160
		}
	}
	for range 40 {
		x0, y0 := r.IntN(w-40), r.IntN(h-40)
		x1, y1 := x0+10+r.IntN(80), y0+10+r.IntN(60)
		c := uint8(r.IntN(256)) //nolint:gosec // 0..255
		for y := y0; y < min(y1, h); y++ {
			for x := x0; x < min(x1, w); x++ {
				img.SetGray(x, y, color.Gray{Y: c})
			}
		}
	}
	return img
}

// blur averages over a (2k+1)² box.
func blur(src *image.Gray, k int) *image.Gray {
	b := src.Bounds()
	out := image.NewGray(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			sum, n := 0, 0
			for dy := -k; dy <= k; dy++ {
				for dx := -k; dx <= k; dx++ {
					if p := (image.Point{X: x + dx, Y: y + dy}); p.In(b) {
						sum += int(src.GrayAt(p.X, p.Y).Y)
						n++
					}
				}
			}
			out.SetGray(x, y, color.Gray{Y: uint8(sum / n)}) //nolint:gosec // an average of bytes
		}
	}
	return out
}

// shade scales brightness and adds offset.
func shade(src *image.Gray, f float64, off int) *image.Gray {
	out := image.NewGray(src.Bounds())
	for i, v := range src.Pix {
		out.Pix[i] = uint8(min(255, max(0, int(float64(v)*f)+off)))
	}
	return out
}

func noisy(src *image.Gray, seed uint64) *image.Gray {
	r := rand.New(rand.NewPCG(seed, 99)) //nolint:gosec // test noise
	out := image.NewGray(src.Bounds())
	for i, v := range src.Pix {
		out.Pix[i] = uint8(min(255, max(0, int(v)+r.IntN(7)-3)))
	}
	return out
}

func encode(t *testing.T, img image.Image) []byte {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// fakeCamera serves one picture at a vendor's path with digest auth, and
// an ONVIF device and media service.
type fakeCamera struct {
	*httptest.Server
	mu      sync.Mutex
	picture []byte
	path    string // vendor snapshot path served
	user    string
	pass    string
}

func newCamera(t *testing.T, path string) *fakeCamera {
	f := &fakeCamera{path: path, user: "admin", pass: "cam-secret"}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeCamera) set(b []byte) {
	f.mu.Lock()
	f.picture = b
	f.mu.Unlock()
}

func (f *fakeCamera) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Digest ") {
		return false
	}
	p := parseParams(strings.TrimPrefix(h, "Digest "))
	sum := func(s string) string { x := md5.Sum([]byte(s)); return hex.EncodeToString(x[:]) } //nolint:gosec // digest auth
	ha1 := sum(f.user + ":camera:" + f.pass)
	ha2 := sum(r.Method + ":" + p["uri"])
	return p["username"] == f.user && p["nonce"] == "n0nce" &&
		p["response"] == sum(ha1+":"+p["nonce"]+":"+p["nc"]+":"+p["cnonce"]+":"+p["qop"]+":"+ha2)
}

func (f *fakeCamera) serve(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Digest realm="camera", nonce="n0nce", qop="auth", algorithm=MD5`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.RequestURI() == f.path:
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(f.picture)
	case r.URL.Path == "/onvif/device_service" && f.path == "/onvif-snapshot":
		w.Header().Set("Content-Type", soapContentType)
		_, _ = fmt.Fprintf(w, `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tt="http://www.onvif.org/ver10/schema"><s:Body>
			<tds:GetCapabilitiesResponse xmlns:tds="http://www.onvif.org/ver10/device/wsdl"><tds:Capabilities>
			<tt:Media><tt:XAddr>%s/onvif/media</tt:XAddr></tt:Media></tds:Capabilities></tds:GetCapabilitiesResponse></s:Body></s:Envelope>`, f.URL)
	case r.URL.Path == "/onvif/media" && f.path == "/onvif-snapshot":
		w.Header().Set("Content-Type", soapContentType)
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)
		if strings.Contains(body.String(), "GetProfiles") {
			_, _ = fmt.Fprint(w, `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><trt:GetProfilesResponse xmlns:trt="http://www.onvif.org/ver10/media/wsdl">
				<trt:Profiles token="main_1" fixed="true"><tt:Name xmlns:tt="http://www.onvif.org/ver10/schema">main</tt:Name></trt:Profiles></trt:GetProfilesResponse></s:Body></s:Envelope>`)
			return
		}
		if !strings.Contains(body.String(), "<trt:ProfileToken>main_1</trt:ProfileToken>") || !strings.Contains(body.String(), "PasswordDigest") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintf(w, `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body><trt:GetSnapshotUriResponse xmlns:trt="http://www.onvif.org/ver10/media/wsdl">
			<trt:MediaUri><tt:Uri xmlns:tt="http://www.onvif.org/ver10/schema">%s/onvif-snapshot</tt:Uri></trt:MediaUri></trt:GetSnapshotUriResponse></s:Body></s:Envelope>`, f.URL)
	default:
		http.NotFound(w, r)
	}
}

func creds(pass string) map[string]plugin.Credential {
	return map[string]plugin.Credential{"auth": {Type: "rtsp", Fields: map[string]string{"username": "admin"},
		Secret: map[string]plugin.Secret{"password": plugin.Secret(pass)}}}
}

func run(t *testing.T, f *fakeCamera, cfg any, st plugin.StateStore) plugin.Result {
	t.Helper()
	return plugintest.Run(t, &Snapshot{}, plugintest.Options{Address: strings.TrimPrefix(f.URL, "http://"), Config: cfg,
		Credentials: creds(f.pass), State: st, Timeout: 10 * time.Second})
}

func TestPictureChecks(t *testing.T) {
	base := scene(1, 640, 480)
	f := newCamera(t, "/ISAPI/Streaming/channels/101/picture")
	st := &plugin.MemState{}
	frame := uint64(0)
	show := func(img *image.Gray) plugin.Result {
		t.Helper()
		frame++
		f.set(encode(t, noisy(img, frame))) // sensor noise: consecutive pictures differ slightly
		return run(t, f, nil, st)
	}

	r := show(base)
	if r.Status != plugin.OK || !strings.Contains(r.Output, "from hikvision") || r.Metrics[mWidth] != 640 {
		t.Fatalf("first picture: %v %q", r.Status, r.Output)
	}
	r = show(base)
	if r.Status != plugin.OK || r.Metrics[mSharpnessRef] < 80 || r.Metrics[mSceneChange] > 10 {
		t.Fatalf("same scene: %v %q %v", r.Status, r.Output, r.Metrics)
	}
	r = show(blur(base, 3))
	if r.Status != plugin.Warning || !strings.Contains(r.Output, "blurred") {
		t.Errorf("blurred: %v %q sharpness %.0f %%", r.Status, r.Output, r.Metrics[mSharpnessRef])
	}
	r = show(shade(base, 0.15, 0))
	if r.Status != plugin.Warning || !strings.Contains(r.Output, "too dark") || strings.Contains(r.Output, "blurred") {
		t.Errorf("dark: %v %q", r.Status, r.Output)
	}
	r = show(shade(base, 0.2, 230))
	if r.Status != plugin.Warning || !strings.Contains(r.Output, "overexposed") {
		t.Errorf("overexposed: %v %q", r.Status, r.Output)
	}
	r = show(shade(base, 0, 90))
	if r.Status != plugin.Critical || !strings.Contains(r.Output, "covered") {
		t.Errorf("covered: %v %q", r.Status, r.Output)
	}
	r = show(shade(base, 0.03, 0)) // black: no light at all, or a covered lens
	if r.Status != plugin.Critical || !strings.Contains(r.Output, "covered") {
		t.Errorf("black: %v %q", r.Status, r.Output)
	}
	r = show(scene(2, 640, 480))
	if r.Status != plugin.Warning || !strings.Contains(r.Output, "differs") {
		t.Errorf("moved: %v %q change %.0f %%", r.Status, r.Output, r.Metrics[mSceneChange])
	}

	// Frozen: the very same picture three times.
	same := encode(t, base)
	f.set(same)
	for i := range 3 {
		r = run(t, f, nil, st)
		if i < 2 && strings.Contains(r.Output, "frozen") {
			t.Fatalf("frozen after %d", i+1)
		}
	}
	if r.Status != plugin.Critical || !strings.Contains(r.Output, "frozen: the same picture 3 times") {
		t.Errorf("frozen: %v %q", r.Status, r.Output)
	}

	// A new reference_id learns the moved scene as the reference.
	f.set(encode(t, noisy(scene(2, 640, 480), 500)))
	r = run(t, f, Config{ReferenceID: "re-aimed"}, st)
	f.set(encode(t, noisy(scene(2, 640, 480), 501)))
	r = run(t, f, Config{ReferenceID: "re-aimed"}, st)
	if r.Status != plugin.OK {
		t.Errorf("after a new reference: %v %q", r.Status, r.Output)
	}
	// Checks can be turned off.
	f.set(encode(t, shade(base, 0, 90)))
	if r = run(t, f, Config{Checks: []string{"blur"}}, &plugin.MemState{}); r.Status != plugin.OK {
		t.Errorf("covered check off: %v %q", r.Status, r.Output)
	}
}

func TestSources(t *testing.T) {
	pic := encode(t, scene(3, 320, 240))
	for _, tc := range []struct {
		path, want string
		cfg        any
	}{
		{"/cgi-bin/snapshot.cgi?channel=1", "dahua", nil},
		{"/axis-cgi/jpg/image.cgi", "axis", nil},
		{"/onvif-snapshot", "onvif", nil},
		{"/ISAPI/Streaming/channels/201/picture", "hikvision", Config{Vendor: "hikvision", Channel: 2}},
		{"/custom/snap.jpg", "snapshot_url", Config{SnapshotURL: "/custom/snap.jpg"}},
	} {
		t.Run(tc.want, func(t *testing.T) {
			f := newCamera(t, tc.path)
			f.set(pic)
			st := &plugin.MemState{}
			r := run(t, f, tc.cfg, st)
			if r.Status != plugin.OK || !strings.Contains(r.Output, "from "+tc.want) {
				t.Fatalf("got %v %q", r.Status, r.Output)
			}
			if tc.cfg == nil { // auto remembers what worked
				if b, _ := st.Get("camera"); !strings.Contains(string(b), `"vendor":"`+tc.want+`"`) {
					t.Errorf("state: %s", b)
				}
			}
		})
	}
}

func TestFailures(t *testing.T) {
	f := newCamera(t, "/axis-cgi/jpg/image.cgi")
	f.set(encode(t, scene(4, 320, 240)))
	addr := strings.TrimPrefix(f.URL, "http://")
	r := plugintest.Run(t, &Snapshot{}, plugintest.Options{Address: addr, Credentials: creds("wrong")})
	if r.Status != plugin.Unknown || !strings.Contains(r.Output, "rejected the credential") {
		t.Errorf("wrong password: %v %q", r.Status, r.Output)
	}
	r = plugintest.Run(t, &Snapshot{}, plugintest.Options{Address: "127.0.0.1:1", Credentials: creds("x"), Timeout: 3 * time.Second})
	if r.Status != plugin.Critical {
		t.Errorf("unreachable: %v %q", r.Status, r.Output)
	}
	r = plugintest.Run(t, &Snapshot{}, plugintest.Options{Address: addr, Credentials: creds(f.pass), Network: &plugin.NetPolicy{DenyPrivate: true}})
	if r.Status != plugin.Unknown || !strings.Contains(r.Output, "private or local") {
		t.Errorf("policy: %v %q", r.Status, r.Output)
	}
	f.path = "/nowhere"
	r = plugintest.Run(t, &Snapshot{}, plugintest.Options{Address: addr, Credentials: creds(f.pass)})
	if r.Status != plugin.Unknown || !strings.Contains(r.Output, "set vendor or snapshot_url") {
		t.Errorf("no snapshot URL: %v %q", r.Status, r.Output)
	}
	for _, raw := range []string{`{"checks":["focus"]}`, `{"vendor":"url"}`, `{"snapshot_url":"ftp://x/y"}`, `{"snapshot_url":"http://u:p@cam/snap"}`} {
		if err := (&Snapshot{}).Validate([]byte(raw)); err == nil {
			t.Errorf("%s accepted", raw)
		}
	}
}
