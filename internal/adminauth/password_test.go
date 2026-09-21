package adminauth

import (
	"strings"
	"testing"
)

func TestPHCParserBounds(t *testing.T) {
	valid, err := Hash("a-valid-test-password", TestParams)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"wrong algorithm":   strings.Replace(valid, "argon2id", "argon2i", 1),
		"wrong version":     strings.Replace(valid, "v=19", "v=16", 1),
		"missing field":     valid[:strings.LastIndex(valid, "$")],
		"reordered t,m,p":   "$argon2id$v=19$t=3,m=65536,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"reordered p,t,m":   "$argon2id$v=19$p=2,t=3,m=65536$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"reordered m,p,t":   "$argon2id$v=19$m=65536,p=2,t=3$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"zero memory":       "$argon2id$v=19$m=0,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"absurd memory":     "$argon2id$v=19$m=99999999,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"absurd iterations": "$argon2id$v=19$m=65536,t=999,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"duplicate field":   "$argon2id$v=19$m=65536,m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"short salt":        "$argon2id$v=19$m=8192,t=1,p=1$AAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"short key":         "$argon2id$v=19$m=8192,t=1,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAA",
		"bad base64":        "$argon2id$v=19$m=8192,t=1,p=1$AAA!!!AA!!!AAA!!!A$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}
	for name, phc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePHC(phc); err == nil {
				t.Fatalf("%s: parser accepted %q", name, phc)
			}
			if Verify("whatever", phc) {
				t.Fatalf("%s: Verify accepted %q", name, phc)
			}
		})
	}
}

func TestPasswordPolicyAndVerify(t *testing.T) {
	for name, pw := range map[string]string{
		"too short": "short",
		"NUL":       "validlengthpass\x00word",
		"bad UTF-8": string([]byte{'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 0xff, 0xfe, 'x', 'y'}),
		"too long":  strings.Repeat("x", 257),
	} {
		if err := ValidatePassword(pw); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if err := ValidatePassword("exactly-12ch"); err != nil {
		t.Fatalf("12-byte password rejected: %v", err)
	}
	// A legitimate U+FFFD (encoded correctly) is valid UTF-8 — the policy
	// rejects malformed byte sequences, not code points.
	if err := ValidatePassword("valid-pass-\uFFFD"); err != nil {
		t.Fatalf("valid U+FFFD password rejected: %v", err)
	}
	if err := ValidatePassword("ok-length-here\xff\xfe"); err == nil {
		t.Fatal("malformed UTF-8 tail accepted")
	}
	hash, err := Hash("correct horse battery", TestParams)
	if err != nil {
		t.Fatal(err)
	}
	if !Verify("correct horse battery", hash) {
		t.Fatal("Verify rejected the correct password")
	}
	if Verify("wrong password 123", hash) {
		t.Fatal("Verify accepted a wrong password")
	}
	if err := ProductionParams.Validate(); err != nil {
		t.Fatalf("production params invalid: %v", err)
	}
}
