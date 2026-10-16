package pairing

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// testdata/pairing.json was made by the app (Dart) and sealed by both
// sides: each must read the other's pairing codes and sealed messages.
type vector struct {
	Pairing      Pairing        `json:"pairing"`
	Code         string         `json:"code"`
	ClusterID    string         `json:"clusterId"`
	Message      map[string]any `json:"message"`
	SealedByDart string         `json:"sealedByDart"`
	SealedByGo   string         `json:"sealedByGo"`
}

func load(t *testing.T) vector {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/pairing.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestCodesAndIDsMatchTheApp(t *testing.T) {
	v := load(t)
	if got := v.Pairing.Code(); got != v.Code {
		t.Fatalf("code\n got %s\nwant %s", got, v.Code)
	}
	back, err := ParseCode(v.Code)
	if err != nil || back != v.Pairing {
		t.Fatalf("parse: %+v %v", back, err)
	}
	if v.Pairing.ClusterID() != v.ClusterID {
		t.Fatalf("cluster id %s", v.Pairing.ClusterID())
	}
	if _, err := ParseCode("nonsense"); err == nil {
		t.Fatal("parsed nonsense")
	}
}

func TestOpensWhatEitherSideSealed(t *testing.T) {
	v := load(t)
	for name, sealed := range map[string]string{"dart": v.SealedByDart, "go": v.SealedByGo} {
		var got map[string]any
		if err := Open(v.Pairing.SealKey, sealed, &got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(got, v.Message) {
			t.Fatalf("%s: %v", name, got)
		}
	}
}

func TestSealedOnlyOpensWithTheRightKey(t *testing.T) {
	p, other := Generate("https://r", "c"), Generate("https://r", "c")
	s, err := Seal(p.SealKey, map[string]string{"hello": "world"})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := Open(other.SealKey, s, &out); err == nil {
		t.Fatal("opened with the wrong key")
	}
	if err := Open(p.SealKey, s, &out); err != nil || out["hello"] != "world" {
		t.Fatalf("%v %v", out, err)
	}
}

func TestSecretRoundTrip(t *testing.T) {
	p := Generate("https://relay.example/", "prod")
	if p.RelayURL != "https://relay.example" {
		t.Fatalf("slash kept: %s", p.RelayURL)
	}
	back, ok := FromSecretData(p.SecretData())
	if !ok || back != p {
		t.Fatalf("%+v", back)
	}
	if string(p.SecretData()["pairing-code"]) != p.Code() {
		t.Fatal("pairing code missing")
	}
}

// TestWriteVector adds a Go-sealed message to the vector when asked:
//
//	CLUSTERCTL_WRITE_VECTOR=1 go test ./internal/pairing -run WriteVector
func TestWriteVector(t *testing.T) {
	if os.Getenv("CLUSTERCTL_WRITE_VECTOR") == "" {
		t.Skip("set CLUSTERCTL_WRITE_VECTOR=1 to regenerate")
	}
	raw, _ := os.ReadFile("../../testdata/pairing.json")
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	s, err := Seal(v["pairing"].(map[string]any)["sealKey"].(string), v["message"])
	if err != nil {
		t.Fatal(err)
	}
	v["sealedByGo"] = s
	out, _ := json.MarshalIndent(v, "", " ")
	if err := os.WriteFile("../../testdata/pairing.json", append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
