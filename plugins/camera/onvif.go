package camera

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // ONVIF WS-Security UsernameToken digest is defined with SHA-1
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ONVIF: the device service gives the media service's address; the media
// service lists profiles and the snapshot URI of each. Requests carry a
// WS-Security UsernameToken (password digest); most cameras also accept
// HTTP digest, which the client answers when challenged.

const soapContentType = "application/soap+xml; charset=utf-8"

func (c *httpClient) soap(ctx context.Context, url, body string) ([]byte, error) {
	env := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:tds="http://www.onvif.org/ver10/device/wsdl" xmlns:trt="http://www.onvif.org/ver10/media/wsdl" xmlns:tt="http://www.onvif.org/ver10/schema">` +
		c.wsse() + `<s:Body>` + body + `</s:Body></s:Envelope>`
	b, _, err := c.do(ctx, "POST", url, soapContentType, []byte(env))
	return b, err
}

// wsse is the WS-Security header: digest = base64(sha1(nonce + created + password)).
func (c *httpClient) wsse() string {
	if c.user == "" {
		return ""
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	created := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	h := sha1.New() //nolint:gosec // see the import
	h.Write(nonce)
	h.Write([]byte(created))
	h.Write([]byte(c.pass))
	var user bytes.Buffer
	_ = xml.EscapeText(&user, []byte(c.user))
	return `<s:Header><Security s:mustUnderstand="1" xmlns="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd">` +
		`<UsernameToken><Username>` + user.String() + `</Username>` +
		`<Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">` +
		base64.StdEncoding.EncodeToString(h.Sum(nil)) + `</Password>` +
		`<Nonce EncodingType="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary">` +
		base64.StdEncoding.EncodeToString(nonce) + `</Nonce>` +
		`<Created xmlns="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">` + created + `</Created>` +
		`</UsernameToken></Security></s:Header>`
}

// onvifSnapshotURI asks the camera at base (scheme://host[:port]) for the
// snapshot URI of its first media profile.
func (c *httpClient) onvifSnapshotURI(ctx context.Context, base string) (string, error) {
	caps, err := c.soap(ctx, base+"/onvif/device_service", `<tds:GetCapabilities><tds:Category>Media</tds:Category></tds:GetCapabilities>`)
	if err != nil {
		return "", fmt.Errorf("ONVIF GetCapabilities: %w", err)
	}
	media := firstText(caps, "Media", "XAddr")
	if media == "" {
		media = base + "/onvif/media_service"
	}
	profiles, err := c.soap(ctx, media, `<trt:GetProfiles/>`)
	if err != nil {
		return "", fmt.Errorf("ONVIF GetProfiles: %w", err)
	}
	token := firstAttr(profiles, "Profiles", "token")
	if token == "" {
		return "", errors.New("ONVIF: the camera has no media profile")
	}
	var esc bytes.Buffer
	_ = xml.EscapeText(&esc, []byte(token))
	snap, err := c.soap(ctx, media, `<trt:GetSnapshotUri><trt:ProfileToken>`+esc.String()+`</trt:ProfileToken></trt:GetSnapshotUri>`)
	if err != nil {
		return "", fmt.Errorf("ONVIF GetSnapshotUri: %w", err)
	}
	uri := firstText(snap, "MediaUri", "Uri")
	if uri == "" {
		return "", errors.New("ONVIF: the camera gave no snapshot URI")
	}
	return uri, nil
}

// firstText returns the text of the first element named child inside an
// element named parent (local names, any namespace).
func firstText(doc []byte, parent, child string) string {
	d := xml.NewDecoder(bytes.NewReader(doc))
	depth := 0 // > 0 inside parent
	for {
		tok, err := d.Token()
		if err != nil {
			return ""
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth > 0 {
				depth++
				if t.Name.Local == child {
					var s string
					if d.DecodeElement(&s, &t) == nil {
						return strings.TrimSpace(s)
					}
					depth--
				}
			} else if t.Name.Local == parent {
				depth = 1
			}
		case xml.EndElement:
			if depth > 0 {
				depth--
			}
		}
	}
}

// firstAttr returns an attribute of the first element named name.
func firstAttr(doc []byte, name, attr string) string {
	d := xml.NewDecoder(bytes.NewReader(doc))
	for {
		tok, err := d.Token()
		if err != nil {
			return ""
		}
		if t, ok := tok.(xml.StartElement); ok && t.Name.Local == name {
			for _, a := range t.Attr {
				if a.Name.Local == attr {
					return a.Value
				}
			}
		}
	}
}
