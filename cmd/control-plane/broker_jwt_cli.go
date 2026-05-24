package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
)

// runGenBrokerJWTKeyCLI mints a fresh Ed25519 keypair for the trusted-broker
// JWT path. Prints the values the operator needs to paste into both env
// (REPLAY_BROKER_JWT_PRIVATE_KEY) and the pgmqtt GUC (jwt_public_key) —
// control-plane's autoconfig sets the GUC from the env var, so in the
// common path the operator only sets the private side.
//
// We print the 32-byte seed (not the 64-byte expanded private key) because
// ed25519.NewKeyFromSeed deterministically derives the rest. Two encodings
// for the public key because pgmqtt's autoconfig sends base64url, but
// operators copy/pasting into ALTER SYSTEM manually find base64-PEM easier.
func runGenBrokerJWTKeyCLI(_ []string) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen-broker-jwt-key:", err)
		os.Exit(1)
	}
	seed := priv.Seed()
	fmt.Println("# Fresh Ed25519 keypair for pgmqtt Enterprise JWT auth.")
	fmt.Println("# Paste the private key into your .env; the public key is auto-pushed")
	fmt.Println("# to pgmqtt's jwt_public_key GUC at control-plane boot.")
	fmt.Println()
	fmt.Printf("REPLAY_BROKER_JWT_PRIVATE_KEY=%s\n", base64.StdEncoding.EncodeToString(seed))
	fmt.Println()
	fmt.Println("# Public key (for reference / manual GUC configuration):")
	fmt.Printf("#   base64url: %s\n", base64.RawURLEncoding.EncodeToString(pub))
	fmt.Printf("#   ALTER SYSTEM SET pgmqtt.jwt_public_key = '%s';\n", base64.RawURLEncoding.EncodeToString(pub))
}
