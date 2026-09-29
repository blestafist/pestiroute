package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "gateway: scaffold only; no configured service is running (startup planned in FND-002)")
	os.Exit(1)
}
