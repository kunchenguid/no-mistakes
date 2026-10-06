package main

import (
	"fmt"
	"io"
	"os"

	"github.com/kunchenguid/no-mistakes/internal/safepath"
)

func main() {
	data, err := io.ReadAll(os.Stdin)
	if err == nil {
		_, err = os.Stdout.WriteString(safepath.RedactText(string(data)))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
