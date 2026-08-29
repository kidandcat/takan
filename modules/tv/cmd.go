package tv

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"
	"unicode"
)

const (
	restPort = 8001
	wsPort   = 8002
	curlMax  = 8 // seconds; keep agent-side REST short
	wsMax    = 8 // samsungtvws socket timeout; pairing must not hang the hub
)

var (
	machineRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	appIDRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	tokenPathRe  = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,255}$`)
	clientNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)
	keyRe        = regexp.MustCompile(`^KEY_[A-Z0-9_]+$`)
	hostNameRe   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)
	macRe        = regexp.MustCompile(`(?i)^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)
)

// Known remote keys (Samsung Tizen). Aliases map to KEY_*.
var keyAliases = map[string]string{
	"power": "KEY_POWER", "off": "KEY_POWER", "on": "KEY_POWER",
	"home": "KEY_HOME", "source": "KEY_SOURCE", "hdmi": "KEY_HDMI",
	"menu": "KEY_MENU", "tools": "KEY_TOOLS", "info": "KEY_INFO",
	"guide": "KEY_GUIDE", "exit": "KEY_EXIT", "return": "KEY_RETURN",
	"back": "KEY_RETURN", "enter": "KEY_ENTER", "ok": "KEY_ENTER",
	"up": "KEY_UP", "down": "KEY_DOWN", "left": "KEY_LEFT", "right": "KEY_RIGHT",
	"volup": "KEY_VOLUP", "voldown": "KEY_VOLDOWN", "volumeup": "KEY_VOLUP",
	"volumedown": "KEY_VOLDOWN", "mute": "KEY_MUTE",
	"chup": "KEY_CHUP", "chdown": "KEY_CHDOWN", "channelup": "KEY_CHUP",
	"channeldown": "KEY_CHDOWN",
	"play":        "KEY_PLAY", "pause": "KEY_PAUSE", "stop": "KEY_STOP",
	"ff": "KEY_FF", "rew": "KEY_REWIND", "rewind": "KEY_REWIND",
	"red": "KEY_RED", "green": "KEY_GREEN", "yellow": "KEY_YELLOW", "blue": "KEY_BLUE",
}

func validMachine(name string) error {
	name = strings.TrimSpace(name)
	if !machineRe.MatchString(name) {
		return fmt.Errorf("invalid machine name %q (use the registered takan-agent name, e.g. mac)", name)
	}
	return nil
}

func validHost(host string) error {
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("TV host required")
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() != nil {
		return nil
	}
	if hostNameRe.MatchString(host) && !strings.Contains(host, "..") {
		return nil
	}
	return fmt.Errorf("invalid TV host %q (IPv4 or hostname)", host)
}

func validTokenPath(p string) error {
	p = strings.TrimSpace(p)
	if !tokenPathRe.MatchString(p) || strings.Contains(p, "..") {
		return fmt.Errorf("invalid token path %q", p)
	}
	return nil
}

func validClientName(name string) error {
	name = strings.TrimSpace(name)
	if !clientNameRe.MatchString(name) {
		return fmt.Errorf("invalid client name %q", name)
	}
	return nil
}

func validAppID(id string) error {
	id = strings.TrimSpace(id)
	if !appIDRe.MatchString(id) {
		return fmt.Errorf("invalid app id %q", id)
	}
	return nil
}

func validMAC(mac string) error {
	mac = strings.TrimSpace(mac)
	if !macRe.MatchString(mac) {
		return fmt.Errorf("invalid MAC %q (use AA:BB:CC:DD:EE:FF)", mac)
	}
	return nil
}

func looksLikeAppID(s string) bool {
	if validAppID(s) != nil {
		return false
	}
	hasDigit := false
	hasDot := false
	for _, r := range s {
		if r >= '0' && r <= '9' {
			hasDigit = true
		}
		if r == '.' {
			hasDot = true
		}
	}
	return hasDigit || hasDot
}

// NormalizeKey maps a friendly name or KEY_* to a Samsung remote key.
func NormalizeKey(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("key required")
	}
	compact := strings.ToLower(strings.ReplaceAll(s, " ", ""))
	compact = strings.ReplaceAll(compact, "-", "")
	if mapped, ok := keyAliases[compact]; ok {
		return mapped, nil
	}
	up := strings.ToUpper(strings.ReplaceAll(s, " ", "_"))
	if !strings.HasPrefix(up, "KEY_") {
		up = "KEY_" + up
	}
	if !keyRe.MatchString(up) || len(up) > 40 {
		return "", fmt.Errorf("invalid remote key %q", raw)
	}
	return up, nil
}

func restBase(host string) (string, error) {
	if err := validHost(host); err != nil {
		return "", err
	}
	return fmt.Sprintf("http://%s:%d/api/v2/", host, restPort), nil
}

// curlStatus is GET /api/v2/ on the TV (no token). Agent-side, 8s cap.
func curlStatus(host string) (string, error) {
	base, err := restBase(host)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("curl -sS -m %d %s", curlMax, shellSingle(base)), nil
}

// curlApp is POST (launch) or DELETE (close) /api/v2/applications/{appId}.
func curlApp(host, method, appID string) (string, error) {
	base, err := restBase(host)
	if err != nil {
		return "", err
	}
	if err := validAppID(appID); err != nil {
		return "", err
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	switch method {
	case "POST", "DELETE", "GET":
	default:
		return "", fmt.Errorf("unsupported REST method %q", method)
	}
	url := base + "applications/" + appID
	return fmt.Sprintf("curl -sS -m %d -X %s %s", curlMax, method, shellSingle(url)), nil
}

// pyKey sends one remote key over the token websocket (port 8002).
func pyKey(host, tokenPath, clientName, key string) (string, error) {
	if err := validWS(host, tokenPath, clientName); err != nil {
		return "", err
	}
	key, err := NormalizeKey(key)
	if err != nil {
		return "", err
	}
	return pyWS(host, tokenPath, clientName, "tv.send_key("+pyStr(key)+")"), nil
}

// pyText types into the TV (websocket). Pairing uses timeout=8 so the hub is not blocked.
func pyText(host, tokenPath, clientName, text string) (string, error) {
	if err := validWS(host, tokenPath, clientName); err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("text required")
	}
	if len([]rune(text)) > 200 {
		return "", fmt.Errorf("text too long (max 200 characters)")
	}
	for _, r := range text {
		if r == 0 || (unicode.IsControl(r) && r != '\n' && r != '\t') {
			return "", fmt.Errorf("text contains control characters")
		}
	}
	return pyWS(host, tokenPath, clientName, "tv.send_text("+pyStr(text)+")"), nil
}

func validWS(host, tokenPath, clientName string) error {
	if err := validHost(host); err != nil {
		return err
	}
	if err := validTokenPath(tokenPath); err != nil {
		return err
	}
	return validClientName(clientName)
}

func pyWS(host, tokenPath, clientName, action string) string {
	// timeout=8: if the TV shows Allow?, the user accepts there; we do not wait.
	body := "from samsungtvws import SamsungTVWS\n" +
		"tv=SamsungTVWS(host=" + pyStr(host) +
		",port=" + fmt.Sprintf("%d", wsPort) +
		",token_file=" + pyStr(tokenPath) +
		",name=" + pyStr(clientName) +
		",timeout=" + fmt.Sprintf("%d", wsMax) + ")\n" +
		action + "\n"
	return "python3 -c " + shellSingle(body)
}

func pyStr(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// shellSingle wraps s in single quotes for a POSIX shell (agent bash).
func shellSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
