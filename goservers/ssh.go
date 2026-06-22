package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

var (
	sshPassword string
	sshPort     string
	sshOnce     sync.Once
)

// StartSSHServer starts the SSH control server in the background.
func StartSSHServer(baseDir string) {
	sshOnce.Do(func() {
		envVars := loadEnv(filepath.Join(baseDir, ".env"))
		sshPassword = envVars["SSH_PASSWORD"]
		if sshPassword == "" {
			sshPassword = envVars["SSH_ROOT_USER_PASSWORD"]
		}
		if sshPassword == "" {
			log.Println("[SSH] SSH_PASSWORD not set — SSH server disabled")
			return
		}

		sshPort = envVars["SSH_PORT"]
		if sshPort == "" {
			sshPort = "2221"
		}

		// Load or generate host key
		keyPath := filepath.Join(baseDir, "ssh_host_key")
		signer, err := loadOrCreateHostKey(keyPath)
		if err != nil {
			log.Printf("[SSH] host key error: %v — SSH disabled", err)
			return
		}

		go runSSHServer(signer)
	})
}

func loadOrCreateHostKey(path string) (ssh.Signer, error) {
	if data, err := os.ReadFile(path); err == nil {
		return ssh.ParsePrivateKey(data)
	}
	// Generate new RSA key
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}
	data := pem.EncodeToMemory(block)
	if err := os.WriteFile(path, data, 0600); err != nil {
		return nil, fmt.Errorf("save key: %w", err)
	}
	log.Println("[SSH] generated new host key")
	return ssh.ParsePrivateKey(data)
}

func newSSHConfig() *ssh.ServerConfig {
	return &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == "root" && string(pass) == sshPassword {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("access denied")
		},
	}
}

func runSSHServer(signer ssh.Signer) {
	config := newSSHConfig()
	config.AddHostKey(signer)

	listener, err := net.Listen("tcp", ":"+sshPort)
	if err != nil {
		log.Printf("[SSH] listen :%s failed: %v", sshPort, err)
		return
	}
	defer listener.Close()
	log.Printf("[SSH] listening on port %s (user=root)", sshPort)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("[SSH] accept: %v", err)
			continue
		}
		go handleSSHConn(conn.(*net.TCPConn), config)
	}
}

func handleSSHConn(conn net.Conn, config *ssh.ServerConfig) {
	// Make a copy of config with host keys
	srv, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer srv.Close()

	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "unknown channel")
			continue
		}
		ch, reqs, err := newChan.Accept()
		if err != nil {
			continue
		}
		go handleSession(ch, reqs)
	}
}

func handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			req.Reply(true, nil)
		case "shell":
			req.Reply(true, nil)
			shellLoop(ch)
			return
		case "exec":
			req.Reply(true, nil)
			cmd := strings.TrimSpace(string(req.Payload))
			if len(cmd) > 4 {
				cmd = cmd[4:]
			}
			execCmd(ch, cmd)
			return
		default:
			req.Reply(false, nil)
		}
	}
}

func shellLoop(ch ssh.Channel) {
	writePrompt := func() {
		fmt.Fprintf(ch, "root@8wbot~# ")
	}

	fmt.Fprintf(ch, "Welcome to 8wbot SSH console (Go)\r\n")
	fmt.Fprintf(ch, "Commands: help | status | version | bot -v | webuser | exit\r\n")
	writePrompt()

	buf := make([]byte, 256)
	var line string

	for {
		n, err := ch.Read(buf)
		if err != nil {
			return
		}
		input := string(buf[:n])

		for i := 0; i < len(input); i++ {
			b := input[i]
			switch {
			case b == '\x03':
				line = ""
				fmt.Fprintf(ch, "^C\r\n")
				writePrompt()
			case b == '\x04': // Ctrl+D / EOF — exit
				fmt.Fprintf(ch, "logout\r\n")
				return
			case b == '\x7f' || b == '\b':
				if len(line) > 0 {
					line = line[:len(line)-1]
					fmt.Fprintf(ch, "\b \b")
				}
			case b == '\r' || b == '\n':
				if line != "" {
					fmt.Fprintf(ch, "\r\n")
					trimmed := strings.TrimSpace(line)
					if trimmed == "exit" || trimmed == "logout" || trimmed == "quit" {
						fmt.Fprintf(ch, "logout\r\n")
						return
					}
					execCmd(ch, trimmed)
					line = ""
				}
				writePrompt()
			default:
				line += string(b)
				fmt.Fprintf(ch, "%c", b) // echo back typed character
			}
		}
	}
}

func execCmd(ch ssh.Channel, cmd string) {
	switch {
	case cmd == "help":
		fmt.Fprintf(ch, "Commands: help | status | version | bot -v | webuser | exit\r\n")
	case cmd == "version" || cmd == "bot -v":
		fmt.Fprintf(ch, "v0.0.1\r\n")
	case cmd == "status":
		resp, err := http.Get("http://127.0.0.1:8888/api/config")
		if err != nil {
			fmt.Fprintf(ch, "server: api unreachable (%v)\r\n", err)
			return
		}
		defer resp.Body.Close()
		fmt.Fprintf(ch, "server: running\r\n")
	case cmd == "exit" || cmd == "logout" || cmd == "quit":
		fmt.Fprintf(ch, "logout\r\n")
	case len(cmd) >= 8 && cmd[:8] == "webuser ":
		if globalDB == nil {
			fmt.Fprintf(ch, "error: database not available\r\n")
			return
		}
		result := handleWebuser(globalDB, cmd[8:])
		fmt.Fprintf(ch, "%s\r\n", result)
	default:
		fmt.Fprintf(ch, "unknown command: %s\r\n", cmd)
	}
}
