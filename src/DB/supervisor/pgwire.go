package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// pgConn is a minimal PostgreSQL v3 frontend, just enough for the health
// handshake, password authentication (SCRAM-SHA-256, md5, cleartext) and
// the simple-query protocol of the statistics sampler. One connection per
// checker cycle carries both the check and the sampling; the deadline set at
// dial time bounds everything.
type pgConn struct {
	conn     net.Conn
	user     string
	authCode uint32 // the authentication request read during the handshake
	authData []byte
}

// pgDial connects, sends the startup message and reads the first response.
// An authentication request ('R') means the postmaster accepts connections,
// which is the readiness signal pg_isready uses; anything else is an error.
func pgDial(host string, port int, user string, timeout time.Duration) (*pgConn, error) {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	c := &pgConn{conn: conn, user: user}
	if err := c.startup(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *pgConn) startup() error {
	payload := binary.BigEndian.AppendUint32(nil, 196608)
	for _, kv := range [][2]string{
		{"user", c.user}, {"database", "postgres"},
		{"application_name", "pgsupervisor"}, {"client_encoding", "UTF8"},
	} {
		payload = append(payload, kv[0]...)
		payload = append(payload, 0)
		payload = append(payload, kv[1]...)
		payload = append(payload, 0)
	}
	payload = append(payload, 0)
	message := binary.BigEndian.AppendUint32(nil, uint32(len(payload)+4))
	message = append(message, payload...)
	if _, err := c.conn.Write(message); err != nil {
		return err
	}
	kind, data, err := c.readMessage()
	if err != nil {
		return err
	}
	switch kind {
	case 'R':
		if len(data) < 4 {
			return errors.New("malformed authentication request")
		}
		c.authCode = binary.BigEndian.Uint32(data[:4])
		c.authData = data[4:]
		return nil
	case 'E':
		return errors.New("postgres not accepting connections: " + pgErrorMessage(data))
	default:
		return fmt.Errorf("postgres not accepting connections (response %q)", kind)
	}
}

func (c *pgConn) close() {
	_ = c.conn.Close()
}

// terminate ends the session cleanly (the sampler authenticated and ran
// queries; a plain close would leave "unexpected EOF" lines in the log).
func (c *pgConn) terminate() {
	_ = c.writeMessage('X', nil)
	_ = c.conn.Close()
}

func (c *pgConn) readMessage() (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(c.conn, header[:]); err != nil {
		return 0, nil, err
	}
	length := int(binary.BigEndian.Uint32(header[1:5]))
	if length < 4 || length > 64<<20 {
		return 0, nil, fmt.Errorf("invalid message length %d", length)
	}
	payload := make([]byte, length-4)
	if _, err := io.ReadFull(c.conn, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

func (c *pgConn) writeMessage(kind byte, payload []byte) error {
	message := make([]byte, 0, 5+len(payload))
	message = append(message, kind)
	message = binary.BigEndian.AppendUint32(message, uint32(len(payload)+4))
	message = append(message, payload...)
	_, err := c.conn.Write(message)
	return err
}

// readAuth reads the next authentication request; an ErrorResponse becomes
// the error (e.g. "password authentication failed").
func (c *pgConn) readAuth() (uint32, []byte, error) {
	kind, data, err := c.readMessage()
	if err != nil {
		return 0, nil, err
	}
	switch kind {
	case 'R':
		if len(data) < 4 {
			return 0, nil, errors.New("malformed authentication request")
		}
		return binary.BigEndian.Uint32(data[:4]), data[4:], nil
	case 'E':
		return 0, nil, errors.New(pgErrorMessage(data))
	default:
		return 0, nil, fmt.Errorf("unexpected message %q during authentication", kind)
	}
}

// authenticate completes the exchange the handshake left pending and waits
// for ReadyForQuery.
func (c *pgConn) authenticate(password string) error {
	code, data := c.authCode, c.authData
	for {
		var err error
		switch code {
		case 0: // AuthenticationOk
			return c.awaitReady()
		case 3: // cleartext
			err = c.writeMessage('p', append([]byte(password), 0))
		case 5: // md5
			if len(data) < 4 {
				return errors.New("malformed md5 authentication request")
			}
			inner := md5.Sum([]byte(password + c.user))
			outer := md5.Sum(append([]byte(hex.EncodeToString(inner[:])), data[:4]...))
			err = c.writeMessage('p', append([]byte("md5"+hex.EncodeToString(outer[:])), 0))
		case 10: // SASL
			err = c.scramSHA256(data, password)
		default:
			return fmt.Errorf("unsupported authentication method %d", code)
		}
		if err != nil {
			return err
		}
		if code, data, err = c.readAuth(); err != nil {
			return err
		}
	}
}

func (c *pgConn) awaitReady() error {
	for {
		kind, data, err := c.readMessage()
		if err != nil {
			return err
		}
		switch kind {
		case 'S', 'K', 'N':
		case 'Z':
			return nil
		case 'E':
			return errors.New(pgErrorMessage(data))
		default:
			return fmt.Errorf("unexpected message %q before ReadyForQuery", kind)
		}
	}
}

// scramSHA256 runs the SCRAM-SHA-256 exchange (RFC 5802/7677) over the SASL
// messages of the PostgreSQL protocol. The username attribute is sent empty,
// as libpq does: the server uses the startup message's user. Passwords are
// used as given (no SASLprep), which is exact for ASCII passwords.
func (c *pgConn) scramSHA256(mechanisms []byte, password string) error {
	offered := false
	for _, mechanism := range bytes.Split(mechanisms, []byte{0}) {
		if string(mechanism) == "SCRAM-SHA-256" {
			offered = true
		}
	}
	if !offered {
		return errors.New("server does not offer SCRAM-SHA-256")
	}
	nonceRaw := make([]byte, 18)
	if _, err := rand.Read(nonceRaw); err != nil {
		return err
	}
	clientNonce := base64.StdEncoding.EncodeToString(nonceRaw)
	clientFirstBare := "n=,r=" + clientNonce
	clientFirst := "n,," + clientFirstBare
	initial := append([]byte("SCRAM-SHA-256"), 0)
	initial = binary.BigEndian.AppendUint32(initial, uint32(len(clientFirst)))
	initial = append(initial, clientFirst...)
	if err := c.writeMessage('p', initial); err != nil {
		return err
	}

	code, data, err := c.readAuth()
	if err != nil {
		return err
	}
	if code != 11 {
		return fmt.Errorf("expected SASL continue, got authentication code %d", code)
	}
	serverFirst := string(data)
	attrs := scramAttributes(serverFirst)
	serverNonce := attrs["r"]
	if !strings.HasPrefix(serverNonce, clientNonce) || len(serverNonce) == len(clientNonce) {
		return errors.New("SCRAM server nonce does not extend the client nonce")
	}
	salt, err := base64.StdEncoding.DecodeString(attrs["s"])
	if err != nil {
		return fmt.Errorf("SCRAM salt: %w", err)
	}
	iterations, err := strconv.Atoi(attrs["i"])
	if err != nil || iterations < 1 {
		return errors.New("SCRAM iteration count invalid")
	}
	saltedPassword, err := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if err != nil {
		return err
	}
	clientKey := hmacSHA256(saltedPassword, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)
	clientFinalWithoutProof := "c=biws,r=" + serverNonce
	authMessage := clientFirstBare + "," + serverFirst + "," + clientFinalWithoutProof
	clientSignature := hmacSHA256(storedKey[:], []byte(authMessage))
	proof := make([]byte, len(clientKey))
	for i := range clientKey {
		proof[i] = clientKey[i] ^ clientSignature[i]
	}
	clientFinal := clientFinalWithoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof)
	if err := c.writeMessage('p', []byte(clientFinal)); err != nil {
		return err
	}

	code, data, err = c.readAuth()
	if err != nil {
		return err
	}
	if code != 12 {
		return fmt.Errorf("expected SASL final, got authentication code %d", code)
	}
	attrs = scramAttributes(string(data))
	if serverError, present := attrs["e"]; present {
		return errors.New("SCRAM server error: " + serverError)
	}
	serverKey := hmacSHA256(saltedPassword, []byte("Server Key"))
	expected := base64.StdEncoding.EncodeToString(hmacSHA256(serverKey, []byte(authMessage)))
	if subtle.ConstantTimeCompare([]byte(attrs["v"]), []byte(expected)) != 1 {
		return errors.New("SCRAM server signature mismatch")
	}
	return nil
}

func scramAttributes(message string) map[string]string {
	attrs := map[string]string{}
	for _, part := range strings.Split(message, ",") {
		if key, value, found := strings.Cut(part, "="); found {
			attrs[key] = value
		}
	}
	return attrs
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// query runs one statement through the simple-query protocol and returns
// the rows as text columns (SQL NULL becomes the empty string).
func (c *pgConn) query(sql string) ([][]string, error) {
	if err := c.writeMessage('Q', append([]byte(sql), 0)); err != nil {
		return nil, err
	}
	var rows [][]string
	var queryErr error
	for {
		kind, data, err := c.readMessage()
		if err != nil {
			return nil, err
		}
		switch kind {
		case 'T', 'C', 'N', 'I', 'S':
		case 'D':
			row, err := parseDataRow(data)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
		case 'E':
			queryErr = errors.New(pgErrorMessage(data))
		case 'Z':
			return rows, queryErr
		default:
			return nil, fmt.Errorf("unexpected message %q in query response", kind)
		}
	}
}

func parseDataRow(data []byte) ([]string, error) {
	if len(data) < 2 {
		return nil, errors.New("malformed data row")
	}
	count := int(binary.BigEndian.Uint16(data[:2]))
	row := make([]string, 0, count)
	offset := 2
	for i := 0; i < count; i++ {
		if offset+4 > len(data) {
			return nil, errors.New("malformed data row")
		}
		length := int(int32(binary.BigEndian.Uint32(data[offset : offset+4])))
		offset += 4
		if length < 0 {
			row = append(row, "")
			continue
		}
		if offset+length > len(data) {
			return nil, errors.New("malformed data row")
		}
		row = append(row, string(data[offset:offset+length]))
		offset += length
	}
	return row, nil
}

// pgErrorMessage renders an ErrorResponse as "SEVERITY: message (SQLSTATE)".
func pgErrorMessage(data []byte) string {
	fields := map[byte]string{}
	for len(data) > 0 && data[0] != 0 {
		code := data[0]
		end := bytes.IndexByte(data[1:], 0)
		if end < 0 {
			break
		}
		fields[code] = string(data[1 : 1+end])
		data = data[2+end:]
	}
	message := fields['M']
	if message == "" {
		message = "unknown error"
	}
	if fields['S'] != "" {
		message = fields['S'] + ": " + message
	}
	if fields['C'] != "" {
		message += " (" + fields['C'] + ")"
	}
	return message
}
