package credentials

import (
	"encoding/base64"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// buildSSH dials an ssh connection from a kind=ssh credential and returns the
// *ssh.Client (the secret stays here — the caller runs sessions, never sees the
// key). Fields: host, user, port (default 22), one of private_key_b64 (base64
// PEM, optional passphrase) / password.
//
// Host-key check: InsecureIgnoreHostKey — the accept-risk decision (memory
// ssh-hostkey-accept-risk: single-operator LAN; MITM surface accepted). A
// known_hosts pin is a followup.
func buildSSH(data map[string]string) (*ssh.Client, error) {
	host, user := data["host"], data["user"]
	if host == "" || user == "" {
		return nil, fmt.Errorf("ssh credential needs host + user")
	}
	port := data["port"]
	if port == "" {
		port = "22"
	}

	var auths []ssh.AuthMethod
	if kb := data["private_key_b64"]; kb != "" {
		pem, err := base64.StdEncoding.DecodeString(kb)
		if err != nil {
			return nil, fmt.Errorf("private_key_b64: %w", err)
		}
		var signer ssh.Signer
		if pass := data["passphrase"]; pass != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(pem, []byte(pass))
		} else {
			signer, err = ssh.ParsePrivateKey(pem)
		}
		if err != nil {
			return nil, fmt.Errorf("parse private key: %w", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if pw := data["password"]; pw != "" {
		auths = append(auths, ssh.Password(pw))
	}
	if len(auths) == 0 {
		return nil, fmt.Errorf("ssh credential needs private_key_b64 or password")
	}

	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            auths,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // accept-risk (see doc)
		Timeout:         10 * time.Second,
	}
	// ssh.Dial's cfg.Timeout bounds ONLY the TCP connect; the handshake (banner +
	// kex + auth) then runs with no deadline (golang/go#21941), so a wedged/tarpit
	// sshd would hang the calling turn forever. Dial + set a deadline ourselves so
	// connect AND handshake are both bounded, then clear it for the live session.
	addr := net.JoinHostPort(host, port)
	conn, err := net.DialTimeout("tcp", addr, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(cfg.Timeout)); err != nil {
		conn.Close()
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		c.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}
