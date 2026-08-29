package tv

import (
	"encoding/json"
	"strings"
)

func parsePowerAndMAC(raw string) (power, mac string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	var top map[string]any
	if json.Unmarshal([]byte(raw), &top) != nil {
		return "", ""
	}
	dev, _ := top["device"].(map[string]any)
	power = stringFrom(top, "PowerState")
	if power == "" && dev != nil {
		power = stringFrom(dev, "PowerState")
	}
	mac = stringFrom(top, "wifiMac")
	if mac == "" {
		mac = stringFrom(top, "wifiMacAddress")
	}
	if dev != nil {
		if mac == "" {
			mac = stringFrom(dev, "wifiMac")
		}
		if mac == "" {
			mac = stringFrom(dev, "wifiMacAddress")
		}
	}
	power = strings.ToLower(strings.TrimSpace(power))
	mac = strings.ToUpper(strings.TrimSpace(mac))
	if validMAC(mac) != nil {
		mac = ""
	}
	return power, mac
}

func parseAppStatus(raw string) (running, visible bool, name string) {
	var m map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &m) != nil {
		return false, false, ""
	}
	name = stringFrom(m, "name")
	running = boolish(m["running"])
	visible = boolish(m["visible"])
	return running, visible, name
}

func stringFrom(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func boolish(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "true" || s == "1" || s == "yes"
	case float64:
		return t != 0
	default:
		return false
	}
}

func powerOn(power string) bool {
	return strings.EqualFold(strings.TrimSpace(power), "on")
}
