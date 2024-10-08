package ChaCha20

import "fmt"

type ServerHello struct {
	ServerSignature []byte
	ServerNonce     []byte
	CurvePublicKey  []byte
}

func (s *ServerHello) Read(data []byte) (*ServerHello, error) {
	if len(data) < 128 {
		return nil, fmt.Errorf("invalid data")
	}

	s.ServerSignature = data[:64]
	s.ServerNonce = data[64 : 64+32]
	s.CurvePublicKey = data[64+32 : 128]

	return s, nil
}

func (m *ServerHello) Write(signature *[]byte, nonce *[]byte, curvePublicKey *[]byte) (*[]byte, error) {
	if len(*signature) != 64 {
		return nil, fmt.Errorf("invalid signature")
	}
	if len(*nonce) != 32 {
		return nil, fmt.Errorf("invalid nonce")
	}

	if len(*curvePublicKey) != 32 {
		return nil, fmt.Errorf("invalid curve public key")
	}
	arr := make([]byte, len(*signature)+len(*nonce)+len(*curvePublicKey))
	copy(arr, *signature)
	copy(arr[len(*signature):], *nonce)
	copy(arr[len(*signature)+len(*nonce):], *curvePublicKey)

	return &arr, nil
}
