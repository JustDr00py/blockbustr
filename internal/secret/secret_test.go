package secret

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func testKey() []byte {
	k := make([]byte, KeyLen)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	key := testKey()
	blob, err := Seal(key, []byte("rd api key"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Open(key, blob)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "rd api key" {
		t.Errorf("plain = %q", plain)
	}
}

func TestSealIsRandomised(t *testing.T) {
	key := testKey()
	a, _ := Seal(key, []byte("same input"))
	b, _ := Seal(key, []byte("same input"))
	if bytes.Equal(a, b) {
		t.Error("two seals of the same input are identical; nonce reuse")
	}
}

func TestOpenRejectsTamperingAndWrongKey(t *testing.T) {
	key := testKey()
	blob, _ := Seal(key, []byte("payload"))
	blob[0] ^= 0xff
	if _, err := Open(key, blob); err == nil {
		t.Error("tampered blob opened without error")
	}
	other := testKey()
	other[0] ^= 0xff
	clean, _ := Seal(key, []byte("payload"))
	if _, err := Open(other, clean); err == nil {
		t.Error("wrong key opened without error")
	}
}

func TestOpenRejectsShortBlob(t *testing.T) {
	if _, err := Open(testKey(), []byte("short")); err == nil {
		t.Error("short blob opened without error")
	}
}

func TestParseKey(t *testing.T) {
	key := testKey()
	cases := []string{
		hex.EncodeToString(key),
		base64.StdEncoding.EncodeToString(key),
		base64.RawStdEncoding.EncodeToString(key),
		base64.URLEncoding.EncodeToString(key),
		base64.RawURLEncoding.EncodeToString(key),
	}
	for _, c := range cases {
		if got, err := ParseKey(c); err != nil || !bytes.Equal(got, key) {
			t.Errorf("ParseKey(%q) = %v, %v", c, got, err)
		}
	}
	for _, bad := range []string{"", "short", strings.Repeat("a", 63), strings.Repeat("a", 65), "!!not-base64-or-hex!!"} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
}
