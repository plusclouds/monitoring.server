// Package webhook delivers events to webhook endpoints (F06, ADR-0009):
// endpoints and alert routes, routing events to deliveries, and signed
// delivery with retries.
package webhook

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

// secretPrefix marks signing secrets, as in Standard Webhooks and PlusClouds.
const secretPrefix = "whsec_"

// NewSecret returns a random signing secret "whsec_<base64 of 32 bytes>".
func NewSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return secretPrefix + base64.StdEncoding.EncodeToString(b), nil
}

func secretKey(secret string) ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, secretPrefix))
	if err != nil || len(k) == 0 {
		return nil, errors.New("malformed signing secret")
	}
	return k, nil
}

// Sign returns the webhook-signature header value for a message: one
// "v1,<base64 HMAC-SHA256 over id.timestamp.body>" per secret, separated by
// spaces, so receivers holding either secret accept it during rotation.
func Sign(secrets []string, id string, timestamp int64, body []byte) (string, error) {
	msg := make([]byte, 0, len(id)+len(body)+24)
	msg = append(msg, id...)
	msg = append(msg, '.')
	msg = strconv.AppendInt(msg, timestamp, 10)
	msg = append(msg, '.')
	msg = append(msg, body...)
	sigs := make([]string, 0, len(secrets))
	for _, s := range secrets {
		k, err := secretKey(s)
		if err != nil {
			return "", err
		}
		m := hmac.New(sha256.New, k)
		m.Write(msg)
		sigs = append(sigs, "v1,"+base64.StdEncoding.EncodeToString(m.Sum(nil)))
	}
	return strings.Join(sigs, " "), nil
}

// Verify checks a webhook-signature header against a secret, the way a
// receiver does. Used by tests and the delivery log.
func Verify(secret, id string, timestamp int64, body []byte, header string) bool {
	want, err := Sign([]string{secret}, id, timestamp, body)
	if err != nil {
		return false
	}
	for _, sig := range strings.Fields(header) {
		if hmac.Equal([]byte(sig), []byte(want)) {
			return true
		}
	}
	return false
}
