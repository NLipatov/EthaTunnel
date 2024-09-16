package network

import (
	"encoding/binary"
	"etha-tunnel/network/packages"
	"etha-tunnel/network/utils"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"
)

func Serve(tunFile *os.File, listenPort string) error {
	externalIfName, err := utils.GetDefaultIf()
	if err != nil {
		return err
	}

	err = enableNAT(externalIfName)
	if err != nil {
		return fmt.Errorf("Failed enabling NAT: %v", err)
	}
	defer disableNAT(externalIfName)

	err = setupForwarding(tunFile, externalIfName)
	if err != nil {
		return fmt.Errorf("Failed to set up forwarding: %v", err)
	}
	defer clearForwarding(tunFile, externalIfName)

	// Map to keep track of connected clients
	var clients sync.Map

	// Start a goroutine to read from TUN interface and send to clients
	go func() {
		buf := make([]byte, 65535)
		for {
			n, err := tunFile.Read(buf)
			if err != nil {
				log.Printf("Failed to read from TUN: %v", err)
				continue
			}
			packet := buf[:n]

			// Send packet to all connected clients
			clients.Range(func(key, value interface{}) bool {
				conn := value.(net.Conn)
				// Send packet length
				length := uint32(len(packet))
				lengthBuf := make([]byte, 4)
				binary.BigEndian.PutUint32(lengthBuf, length)
				_, err := conn.Write(append(lengthBuf, packet...))
				if err != nil {
					log.Printf("Failed to send packet to client: %v", err)
					clients.Delete(key)
				}
				return true
			})
		}
	}()

	// Listen for incoming client connections
	listener, err := net.Listen("tcp", listenPort)
	if err != nil {
		return fmt.Errorf("Failed to listen on port %s: %v", listenPort, err)
	}
	defer listener.Close()
	log.Printf("Server listening on port %s", listenPort)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Failed to accept connection: %v", err)
			continue
		}
		log.Printf("Client connected: %s", conn.RemoteAddr())
		clients.Store(conn.RemoteAddr(), conn)
		go handleClient(conn, tunFile, &clients)
	}
}

func handleClient(conn net.Conn, tunFile *os.File, clients *sync.Map) {
	defer func() {
		clients.Delete(conn.RemoteAddr())
		conn.Close()
		log.Printf("Client disconnected: %s", conn.RemoteAddr())
	}()

	buf := make([]byte, 65535)
	for {
		// Read packet length
		_, err := io.ReadFull(conn, buf[:4])
		if err != nil {
			if err != io.EOF {
				log.Printf("Failed to read from client: %v", err)
			}
			return
		}
		length := binary.BigEndian.Uint32(buf[:4])
		if length > 65535 {
			log.Printf("Packet too large: %d", length)
			return
		}
		// Read packet
		_, err = io.ReadFull(conn, buf[:length])
		if err != nil {
			log.Printf("Failed to read from client: %v", err)
			return
		}
		packet := buf[:length]

		// Write packet to TUN interface
		err = WriteToTun(tunFile, packet)
		if err != nil {
			log.Printf("Failed to write to TUN: %v", err)
			return
		}
	}
}

func forwardPacket(ipHeader *packages.IPv4Header, packet []byte, tunFile *os.File) error {
	destAddr := ipHeader.DestinationIP.String()
	payloadStart := int(ipHeader.IHL) * 4
	payload := packet[payloadStart:]

	var conn net.Conn
	var err error

	switch ipHeader.Protocol {
	case 1: // ICMP
		conn, err = net.Dial("ip4:icmp", destAddr)
	case 6: // TCP
		dstPort := binary.BigEndian.Uint16(payload[2:4])
		conn, err = net.Dial("tcp", fmt.Sprintf("%s:%d", destAddr, dstPort))
	case 17: // UDP
		dstPort := binary.BigEndian.Uint16(payload[2:4])
		conn, err = net.Dial("udp", fmt.Sprintf("%s:%d", destAddr, dstPort))
	default:
		return fmt.Errorf("Unsupported protocol: %d", ipHeader.Protocol)
	}

	if err != nil {
		return fmt.Errorf("Failed to dial destination %s: %v", destAddr, err)
	}
	defer conn.Close()

	// Send the payload to the destination
	_, err = conn.Write(payload)
	if err != nil {
		return fmt.Errorf("Failed to write to destination %s: %v", destAddr, err)
	}

	// Read the response
	respBuf := make([]byte, 65535)
	n, err := conn.Read(respBuf)
	if err != nil {
		if err != io.EOF {
			return fmt.Errorf("Failed to read from destination %s: %v", destAddr, err)
		}
		return nil
	}

	// Prepare the response packet
	respPacket := make([]byte, int(ipHeader.IHL)*4+n)
	copy(respPacket, packet[:int(ipHeader.IHL)*4])      // Copy original IP header
	copy(respPacket[int(ipHeader.IHL)*4:], respBuf[:n]) // Copy payload

	// Swap source and destination IPs
	copy(respPacket[12:16], ipHeader.DestinationIP.To4())
	copy(respPacket[16:20], ipHeader.SourceIP.To4())

	// Recompute checksum
	binary.BigEndian.PutUint16(respPacket[10:12], 0) // Reset checksum field
	checksum := packages.Checksum(respPacket[:int(ipHeader.IHL)*4])
	binary.BigEndian.PutUint16(respPacket[10:12], checksum)

	// Write response back to TUN interface
	err = WriteToTun(tunFile, respPacket)
	if err != nil {
		return fmt.Errorf("Failed to write response to TUN: %v", err)
	}

	return nil
}

func enableNAT(iface string) error {
	cmd := exec.Command("iptables", "-t", "nat", "-A", "POSTROUTING", "-o", iface, "-j", "MASQUERADE")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Failed to enable NAT on %s: %v, output: %s", iface, err, output)
	}
	return nil
}

func disableNAT(iface string) error {
	cmd := exec.Command("iptables", "-t", "nat", "-D", "POSTROUTING", "-o", iface, "-j", "MASQUERADE")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Failed to disable NAT on %s: %v, output: %s", iface, err, output)
	}
	return nil
}

func setupForwarding(tunFile *os.File, extIface string) error {
	// Get the name of the TUN interface
	tunName := getTunInterfaceName(tunFile)
	if tunName == "" {
		return fmt.Errorf("Failed to get TUN interface name")
	}

	// Set up iptables rules
	cmd := exec.Command("iptables", "-A", "FORWARD", "-i", extIface, "-o", tunName, "-m", "state", "--state",
		"RELATED,ESTABLISHED", "-j", "ACCEPT")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Failed to set up forwarding rule for %s -> %s: %v, output: %s", extIface, tunName, err, output)
	}

	cmd = exec.Command("iptables", "-A", "FORWARD", "-i", tunName, "-o", extIface, "-j", "ACCEPT")
	output, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to set up forwarding rule for %s -> %s: %v, output: %s", tunName, extIface, err, output)
	}
	return nil
}

func clearForwarding(tunFile *os.File, extIface string) error {
	tunName := getTunInterfaceName(tunFile)
	if tunName == "" {
		return fmt.Errorf("Failed to get TUN interface name")
	}

	cmd := exec.Command("iptables", "-D", "FORWARD", "-i", extIface, "-o", tunName, "-m", "state", "--state",
		"RELATED,ESTABLISHED", "-j", "ACCEPT")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Failed to remove forwarding rule for %s -> %s: %v, output: %s", extIface, tunName, err, output)
	}

	cmd = exec.Command("iptables", "-D", "FORWARD", "-i", tunName, "-o", extIface, "-j", "ACCEPT")
	output, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Failed to remove forwarding rule for %s -> %s: %v, output: %s", tunName, extIface, err, output)
	}
	return nil
}

func getTunInterfaceName(tunFile *os.File) string {
	// Since we know the interface name, we can return it directly.
	// Alternatively, you could retrieve it from the file descriptor if needed.
	return "ethatun0"
}
