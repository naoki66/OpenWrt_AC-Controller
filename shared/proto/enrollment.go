package proto

// EnrollmentInfo advertises a key exchange key signed by the controller.
type EnrollmentInfo struct {
	ACID       string `json:"ac_id"`
	Public     string `json:"public"`
	Signature  string `json:"signature"`
	SigningKey string `json:"signing_key,omitempty"`
}

type ManagementClaim struct {
	ACID       string `json:"ac_id"`
	SigningKey string `json:"signing_key"`
	Credential string `json:"credential"`
}
