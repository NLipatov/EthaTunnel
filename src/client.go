package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"etha-tunnel/handshake/ChaCha20"
	"etha-tunnel/network"
	"etha-tunnel/network/utils"
	"etha-tunnel/settings/client"
	"fmt"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
	"io"
	"log"
	"net"
	"strings"
	"sync/atomic"
)

func main() {
	conf, err := (&client.Conf{}).Read()
	if err != nil {
		log.Fatalf("failed to read configuration: %v", err)
	}
	clientConfigurationErr := configureClient(conf)
	if clientConfigurationErr != nil {
		log.Fatalf("failed to configure client: %v", clientConfigurationErr)
	}

	tunFile, err := network.OpenTunByName(conf.IfName)
	if err != nil {
		log.Fatalf("failed to open TUN interface: %v", err)
	}
	defer tunFile.Close()

	conn, err := net.Dial("tcp", conf.ServerTCPAddress)
	if err != nil {
		log.Fatalf("failed to connect to server: %v", err)
	}
	defer conn.Close()
	log.Printf("connected to server at %s", conf.ServerTCPAddress)

	session, err := register(conn, conf)
	if err != nil {
		log.Fatalf("registration failed: %s", err)
	}

	// TUN -> TCP
	go func(session *ChaCha20.Session) {
		buf := make([]byte, 65535)
		for {
			n, err := tunFile.Read(buf)
			if err != nil {
				log.Fatalf("failed to read from TUN: %v", err)
			}

			aad := session.CreateAAD(false, session.C2SCounter)

			encryptedPacket, err := session.Encrypt(buf[:n], aad)
			if err != nil {
				log.Fatalf("failed to encrypt packet: %v", err)
			}

			atomic.AddUint64(&session.C2SCounter, 1)

			length := uint32(len(encryptedPacket))
			lengthBuf := make([]byte, 4)
			binary.BigEndian.PutUint32(lengthBuf, length)
			_, err = conn.Write(append(lengthBuf, encryptedPacket...))
			if err != nil {
				log.Fatalf("failed to write to server: %v", err)
			}
		}
	}(session)

	// TCP -> TUN
	buf := make([]byte, 65535)
	for {
		_, err := io.ReadFull(conn, buf[:4])
		if err != nil {
			if err != io.EOF {
				log.Fatalf("failed to read from server: %v", err)
			}
			return
		}
		length := binary.BigEndian.Uint32(buf[:4])

		if length > 65535 {
			log.Fatalf("packet too large: %d", length)
			return
		}

		_, err = io.ReadFull(conn, buf[:length])
		if err != nil {
			log.Fatalf("failed to read encrypted packet: %v", err)
			return
		}

		aad := session.CreateAAD(true, session.S2CCounter)
		decrypted, err := session.Decrypt(buf[:length], aad)
		if err != nil {
			log.Fatalf("failed to decrypt server packet: %s\n", err)
		}

		atomic.AddUint64(&session.S2CCounter, 1)

		_, err = tunFile.Write(decrypted)
		if err != nil {
			log.Fatalf("failed to write to TUN: %v", err)
			return
		}
	}
}

func register(conn net.Conn, conf *client.Conf) (*ChaCha20.Session, error) {
	edPub, ed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate ed25519 key pair: %s", err)
	}

	var curvePrivate [32]byte
	_, _ = io.ReadFull(rand.Reader, curvePrivate[:])
	curvePublic, _ := curve25519.X25519(curvePrivate[:], curve25519.Basepoint)
	nonce := make([]byte, 32)
	_, _ = io.ReadFull(rand.Reader, nonce)

	rm, err := (&ChaCha20.ClientHello{}).Write(4, strings.Split(conf.IfIP, "/")[0], edPub, &curvePublic, &nonce)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize registration message")
	}

	_, err = conn.Write(*rm)
	if err != nil {
		return nil, fmt.Errorf("failed to notice server on local address: %v", err)
	}

	//Mocked server hello
	sHBuf := make([]byte, 128)
	_, err = conn.Read(sHBuf)
	if err != nil {
		return nil, fmt.Errorf("failed to read server-hello message")
	}

	serverHello, err := (&ChaCha20.ServerHello{}).Read(sHBuf)
	if err != nil {
		return nil, fmt.Errorf("failed to read server-hello message")
	}

	clientConf, err := (&client.Conf{}).Read()
	if err != nil {
		return nil, fmt.Errorf("failed to read client configuration: %s", err)
	}
	serverEdPub, err := base64.StdEncoding.DecodeString(clientConf.ServerEd25519PublicKey)
	if err != nil {
		return nil, fmt.Errorf("invalid ed pub key in configuration: %s", err)
	}
	if !ed25519.Verify(serverEdPub, append(append(serverHello.CurvePublicKey, serverHello.ServerNonce...), nonce...), serverHello.ServerSignature) {
		return nil, fmt.Errorf("server failed signature check")
	}

	clientDataToSign := append(append(curvePublic, nonce...), serverHello.ServerNonce...)
	clientSignature := ed25519.Sign(ed, clientDataToSign)
	cS, err := (&ChaCha20.ClientSignature{}).Write(&clientSignature)
	if err != nil {
		return nil, fmt.Errorf("failed to create client signature message: %s", err)
	}

	_, err = conn.Write(*cS)
	if err != nil {
		return nil, fmt.Errorf("failed to send client signature message: %s", err)
	}

	sharedSecret, _ := curve25519.X25519(curvePrivate[:], serverHello.CurvePublicKey)
	salt := sha256.Sum256(append(serverHello.ServerNonce, nonce...))
	infoSC := []byte("server-to-client") // server-key info
	infoCS := []byte("client-to-server") // client-key info
	serverToClientHKDF := hkdf.New(sha256.New, sharedSecret, salt[:], infoSC)
	clientToServerHKDF := hkdf.New(sha256.New, sharedSecret, salt[:], infoCS)
	keySize := chacha20poly1305.KeySize
	serverToClientKey := make([]byte, keySize)
	_, _ = io.ReadFull(serverToClientHKDF, serverToClientKey)
	clientToServerKey := make([]byte, keySize)
	_, _ = io.ReadFull(clientToServerHKDF, clientToServerKey)

	clientSession, err := ChaCha20.NewSession(clientToServerKey, serverToClientKey, false)
	if err != nil {
		return nil, fmt.Errorf("failed to create client session: %s\n", err)
	}

	clientSession.SessionId = sha256.Sum256(append(sharedSecret, salt[:]...))

	return clientSession, nil
}

func configureClient(conf *client.Conf) error {
	_, _ = utils.DelTun(conf.IfName)
	name, err := network.UpNewTun(conf.IfName)
	if err != nil {
		return fmt.Errorf("failed to create interface %v: %v", conf.IfName, err)
	}
	fmt.Printf("created TUN interface: %v\n", name)

	_, err = utils.AssignTunIP(conf.IfName, conf.IfIP)
	if err != nil {
		return err
	}
	fmt.Printf("sssigned IP %s to interface %s\n", conf.IfIP, conf.IfName)

	_, err = utils.SetDefaultIf(conf.IfName)
	if err != nil {
		return err
	}
	fmt.Printf("set %s as default gateway\n", conf.IfName)

	return nil
}
