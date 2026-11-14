package update

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	for _, v := range []struct {
		a, b string
		n    int
	}{{"v1.10.0", "1.9.9", 1}, {"v1.3.2", "1.3.2", 0}, {"1.3.2", "1.3.3", -1}} {
		n, e := compare(v.a, v.b)
		if e != nil || n != v.n {
			t.Fatal(n, e)
		}
	}
	if _, e := compare("v1.4.0-rc1", "1.3.3"); e == nil {
		t.Fatal("prerelease accepted")
	}
}
func TestVerify(t *testing.T) {
	a := Asset{Size: 3, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("abc")))}
	for _, s := range []string{"abc", "axc", "abcx", "ab"} {
		var buf bytes.Buffer
		if (verifiedCopy(&buf, strings.NewReader(s), a) == nil) != (s == "abc") {
			t.Fatal(s)
		}
	}
}
func TestPick(t *testing.T) {
	r := release{Tag: "v1.3.3"}
	name := "KVMSwitch-Setup.msi"
	r.Assets = []Asset{{Name: name, Size: 32, State: "uploaded", Digest: "sha256:" + strings.Repeat("ab", 32), URL: "https://github.com/" + repo + "/releases/download/v1.3.3/" + name}}
	if _, n, e := r.pick("1.3.3", "windows"); e != nil || n != 0 {
		t.Fatal(n, e)
	}
	if _, _, e := r.pick("1.3.2", "mac"); e == nil {
		t.Fatal("missing mac asset")
	}
	r.Assets[0].URL = "https://example.org/test"
	if _, _, e := r.pick("1.3.2", "windows"); e == nil {
		t.Fatal("foreign asset accepted")
	}
}
