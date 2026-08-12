package o2

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type authorizeSession struct {
	ValidationKey    string `json:"validationKey"`
	OAuthBundle      string `json:"oauthBundle"`
	CookieJSessionID string `json:"jsessionid"`
	DeviceID         string `json:"deviceId"`
	DeviceName       string `json:"deviceName"`
	UserAgent        string `json:"userAgent"`
	EncryptionToken  string `json:"encryptionToken"`
}

// runAuthorize runs the Python helper and returns the session
func (f *Fs) runAuthorize(ctx context.Context, args []string) (any, error) {
	pythonPath := f.opt.PythonPath
	if pythonPath == "" {
		pythonPath = "python3"
	}

	helperPath := f.opt.LoginHelper
	if helperPath == "" {
		candidates := []string{"o2_authorize.py"}
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates,
				filepath.Join(home, "o2cloud-gateway", "o2_authorize.py"),
				filepath.Join(home, "rclone-o2", "backend", "o2", "o2_authorize.py"),
			)
		}
		for _, path := range candidates {
			if _, err := os.Stat(path); err == nil {
				helperPath = path
				break
			}
		}
		if helperPath == "" {
			return nil, fmt.Errorf("o2_authorize.py not found; set login_helper option")
		}
	}

	cmd := exec.CommandContext(ctx, pythonPath, helperPath)
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("authorize helper failed: %s\n%s", err, string(exitErr.Stderr))
		}
		return nil, fmt.Errorf("authorize helper failed: %w", err)
	}

	var session authorizeSession
	if err := json.Unmarshal(output, &session); err != nil {
		return nil, fmt.Errorf("invalid authorize output: %w\nraw: %s", err, string(output))
	}

	if session.ValidationKey == "" || session.OAuthBundle == "" {
		return nil, fmt.Errorf("authorize helper returned empty session")
	}

	// print instructions for the user
	fmt.Printf("Session captured successfully!\n")
	fmt.Printf("validation_key: %s\n", session.ValidationKey)
	fmt.Printf("oauth_bundle:   %s...%s\n", session.OAuthBundle[:minInt(8, len(session.OAuthBundle))], session.OAuthBundle[maxInt(0, len(session.OAuthBundle)-4):])
	fmt.Println()
	fmt.Println("To create the remote, run:")
	fmt.Printf(`  rclone config create o2 o2 validation_key="%s" oauth_bundle="%s"`, session.ValidationKey, session.OAuthBundle)
	if session.CookieJSessionID != "" {
		fmt.Printf(` cookie_jsessionid="%s"`, session.CookieJSessionID)
	}
	if session.DeviceID != "" {
		fmt.Printf(` device_id="%s"`, session.DeviceID)
	}
	if session.DeviceName != "" {
		fmt.Printf(` device_name="%s"`, session.DeviceName)
	}
	if session.UserAgent != "" {
		fmt.Printf(` user_agent="%s"`, session.UserAgent)
	}
	fmt.Println()

	return session, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
