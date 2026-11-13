package proto

import "testing"

func TestHelloReplyProofBindsRequestAndChallenge(t *testing.T) {
	const key, challenge = "derived-site-key", "server-challenge"
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
	sig := HelloReplyProof(key, challenge, nonce, request, response)
	if err := VerifyHelloReply(key, sig, challenge, nonce, request, response); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, nonce, signature string
		request, response      []byte
	}{
		{"different nonce", next, sig, request, response},
		{"different request", nonce, sig, []byte(`{"applied_rev":0}`), response},
		{"different response", nonce, sig, request, []byte(`{"desired_rev":3}`)},
		{"unsigned", nonce, "", request, response},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := VerifyHelloReply(key, tc.signature, challenge, tc.nonce, tc.request, tc.response); err == nil {
				t.Fatal("accepted an invalid response")
			}
		})
	}
	if err := VerifyHelloReply(key, sig, "different-server-challenge", nonce, request, response); err == nil {
		t.Fatal("accepted a reply from another challenge")
	}
	if err := VerifyHelloReply("wrong-key", sig, challenge, nonce, request, response); err == nil {
		t.Fatal("accepted a reply authenticated with a different key")
	}
}
