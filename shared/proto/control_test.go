package proto

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func TestHelloReplySignatureBindsRequestAndChallenge(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := NewControlNonce()
	if err != nil {
		t.Fatal(err)
	}
	next, err := NewControlNonce()
	if err != nil {
		t.Fatal(err)
	}
	request := []byte(`{"applied_rev":1}`)
	response := []byte(`{"desired_rev":2}`)
	sig, err := SignHelloReply(priv, nonce, request, response)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyHelloReply(pub, sig, nonce, request, response); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, nonce, signature string
		request, response      []byte
	}{
		{"different challenge", next, sig, request, response},
		{"different request", nonce, sig, []byte(`{"applied_rev":0}`), response},
		{"different response", nonce, sig, request, []byte(`{"desired_rev":3}`)},
		{"unsigned", nonce, "", request, response},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := VerifyHelloReply(pub, tc.signature, tc.nonce, tc.request, tc.response); err == nil {
				t.Fatal("accepted an invalid response")
			}
		})
	}
}
