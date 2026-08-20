package bitbucket

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestMain handles fake twg CLI dispatch when the test binary is re-executed
// as "twg" (see newFakeTwgClient). Real tests run through m.Run() as usual.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_TWG_MODE") != "" {
		handleFakeTwg()
		return
	}
	os.Exit(m.Run())
}

func handleFakeTwg() {
	args := os.Args[1:]
	key := strings.Join(args, "\x1f")

	if logFile := os.Getenv("FAKE_TWG_LOG"); logFile != "" {
		f, _ := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if f != nil {
			fmt.Fprintln(f, key)
			f.Close()
		}
	}

	respFile := os.Getenv("FAKE_TWG_RESPONSES")
	if respFile == "" {
		fmt.Fprintln(os.Stderr, "FAKE_TWG_RESPONSES not set")
		os.Exit(1)
	}
	data, err := os.ReadFile(respFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var responses map[string]struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exitCode"`
	}
	if err := json.Unmarshal(data, &responses); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	resp, ok := responses[key]
	if !ok {
		fmt.Fprintf(os.Stderr, "no fake twg response registered for args: %v\n", args)
		os.Exit(1)
	}
	fmt.Print(resp.Stdout)
	os.Exit(resp.ExitCode)
}
