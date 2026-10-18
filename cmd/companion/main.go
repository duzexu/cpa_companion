package main

import (
	"log"

	"cpa-linuxdo/internal/companion"
)

func main() {
	if err := companion.Run(); err != nil {
		log.Fatal(err)
	}
}
