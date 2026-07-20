// Command genkey generates an Ed25519 keypair for gofer's run-credential
// authority (ADR 0002) and prints the two base64 values the split tiers consume:
//
//	MINT_PUBLIC_KEY          the web tier's verify-only key (base64 raw 32-byte public)
//	AEI_ED25519_PRIVATE_KEY  the AEI control plane's signing key (base64 raw 64-byte private)
//
// The control plane holds the private half and is the sole minter; gofer's web
// tier holds only the public half and can never mint. Output is KEY=VALUE lines so
// a shell can `eval`/source it. It is dev tooling — not shipped in any image.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
)

func main() {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintln(os.Stderr, "genkey:", err)
		os.Exit(1)
	}
	fmt.Printf("MINT_PUBLIC_KEY=%s\n", base64.StdEncoding.EncodeToString(pub))
	fmt.Printf("AEI_ED25519_PRIVATE_KEY=%s\n", base64.StdEncoding.EncodeToString(priv))
}
