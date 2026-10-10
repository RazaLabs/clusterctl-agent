// Package pairing holds the keys that link phones to a cluster's agent, and
// seals alert details so only paired phones can read them. The formats are
// shared with the kubctl app (testdata/pairing.json checks both agree).
package pairing

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Pairing is everything a phone needs to receive a cluster's alerts.
// RelayKey authenticates the agent and phones to the relay; SealKey
// encrypts alert contents end to end and never leaves the cluster and
// paired phones.
type Pairing struct {
	RelayURL string `json:"relay"`
	RelayKey string `json:"relayKey"`
	SealKey  string `json:"sealKey"`
	// Cluster is the display name shown in notifications.
	Cluster string `json:"cluster"`
}

var b64 = base64.RawURLEncoding

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the OS has no randomness: nothing sensible to do
	}
	return b64.EncodeToString(b)
}

// TrimSlash turns "https://relay.example/" into "https://relay.example".
func TrimSlash(url string) string { return strings.TrimRight(strings.TrimSpace(url), "/") }

// Generate makes fresh keys.
func Generate(relayURL, cluster string) Pairing {
	return Pairing{RelayURL: TrimSlash(relayURL), RelayKey: random(32), SealKey: random(32), Cluster: cluster}
}

// RelayID is what the relay knows a key by: the first 16 bytes of its
// sha256, hex. Never the key itself.
func RelayID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:16])
}

// ClusterID is the cluster's id at the relay.
func (p Pairing) ClusterID() string { return RelayID(p.RelayKey) }

const codePrefix = "kubctl-pair-v1:"

// Code is the pairing code an admin shares with people who can't read the
// agent's Secret.
func (p Pairing) Code() string {
	j, _ := json.Marshal(p)
	return codePrefix + b64.EncodeToString(j)
}

// ParseCode reads a pairing code.
func ParseCode(code string) (Pairing, error) {
	body, ok := strings.CutPrefix(strings.TrimSpace(code), codePrefix)
	if !ok {
		return Pairing{}, errors.New("not a kubctl pairing code")
	}
	raw, err := b64.DecodeString(strings.TrimRight(body, "="))
	if err != nil {
		return Pairing{}, err
	}
	var p Pairing
	err = json.Unmarshal(raw, &p)
	return p, err
}

// Secret keys, as the agent's Secret stores them (values before base64).
const (
	keyRelayURL = "relay-url"
	keyRelayKey = "relay-key"
	keySealKey  = "seal-key"
	keyCluster  = "cluster-name"
	keyCode     = "pairing-code"
)

// SecretData is the Secret's data, including the pairing code.
func (p Pairing) SecretData() map[string][]byte {
	return map[string][]byte{
		keyRelayURL: []byte(p.RelayURL),
		keyRelayKey: []byte(p.RelayKey),
		keySealKey:  []byte(p.SealKey),
		keyCluster:  []byte(p.Cluster),
		keyCode:     []byte(p.Code()),
	}
}

// FromSecretData reads keys from a Secret; false if incomplete.
func FromSecretData(d map[string][]byte) (Pairing, bool) {
	url, relay, seal := d[keyRelayURL], d[keyRelayKey], d[keySealKey]
	if url == nil || relay == nil || seal == nil {
		return Pairing{}, false
	}
	return Pairing{RelayURL: TrimSlash(string(url)), RelayKey: string(relay), SealKey: string(seal), Cluster: string(d[keyCluster])}, true
}

func gcm(sealKey string) (cipher.AEAD, error) {
	key, err := b64.DecodeString(strings.TrimRight(sealKey, "="))
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts message as JSON with AES-256-GCM: a fresh 12-byte nonce,
// then ciphertext and tag, base64url.
func Seal(sealKey string, message any) (string, error) {
	aead, err := gcm(sealKey)
	if err != nil {
		return "", fmt.Errorf("seal key: %w", err)
	}
	plain, err := json.Marshal(message)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return b64.EncodeToString(aead.Seal(nonce, nonce, plain, nil)), nil
}

// Open decrypts what Seal (or the app) sealed into out.
func Open(sealKey, sealed string, out any) error {
	aead, err := gcm(sealKey)
	if err != nil {
		return err
	}
	raw, err := b64.DecodeString(strings.TrimRight(sealed, "="))
	if err != nil {
		return err
	}
	if len(raw) < aead.NonceSize() {
		return errors.New("sealed message too short")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(plain, out)
}
