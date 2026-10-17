// Command keygen creates RSA key pairs for the switch and the simulated banks (local/dev only;
// production keys are generated inside the HSM).
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AppTestify/UPI-Switch/internal/security"
)

func main() {
	dir := flag.String("dir", "keys", "output directory")
	names := flag.String("names", "switch,bank-a,bank-b,bank-c", "comma-separated key names")
	force := flag.Bool("force", false, "overwrite existing keys")
	flag.Parse()
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		panic(err)
	}
	for _, n := range splitComma(*names) {
		keyPath := filepath.Join(*dir, n+".key")
		if _, err := os.Stat(keyPath); err == nil && !*force {
			fmt.Println("exists:", keyPath)
			continue
		}
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(keyPath, security.EncodePrivateKeyPEM(k), 0o600); err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(*dir, n+".pub"), security.EncodePublicKeyPEM(&k.PublicKey), 0o644); err != nil {
			panic(err)
		}
		fmt.Println("wrote:", keyPath)
	}
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}
