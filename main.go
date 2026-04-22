package main

import (
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/ethereum/go-ethereum/crypto"
)

type KeyPair struct {
	PrivateKey string `json:"privateKey"`
	Address    string `json:"address"`
}

func main() {
	http.Handle("/", http.FileServer(http.Dir("./public")))

	http.HandleFunc("/generate", func(w http.ResponseWriter, r *http.Request) {
		privateKey, err := crypto.GenerateKey()
		if err != nil {
			http.Error(w, "Failed to generate key", http.StatusInternalServerError)
			return
		}

		privateKeyBytes := crypto.FromECDSA(privateKey)
		publicKey := privateKey.Public()
		publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
		if !ok {
			http.Error(w, "Failed to cast public key", http.StatusInternalServerError)
			return
		}

		address := crypto.PubkeyToAddress(*publicKeyECDSA).Hex()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(KeyPair{
			PrivateKey: fmt.Sprintf("%x", privateKeyBytes),
			Address:    address,
		})
	})

	log.Println("Server running on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
