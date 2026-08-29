package tv

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	upnpPort = 9197
	upnpPath = "/upnp/control/RenderingControl1"
	upnpNS   = "urn:schemas-upnp-org:service:RenderingControl:1"
)

var upnpActionRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,31}$`)

// curlUPNP POSTs RenderingControl SOAP to http://HOST:9197/upnp/control/RenderingControl1.
func curlUPNP(host, action, extraXML string) (string, error) {
	if err := validHost(host); err != nil {
		return "", err
	}
	if !upnpActionRe.MatchString(action) {
		return "", fmt.Errorf("invalid UPnP action %q", action)
	}
	url := fmt.Sprintf("http://%s:%d%s", host, upnpPort, upnpPath)
	body := `<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body><u:` + action + ` xmlns:u="` + upnpNS + `">` +
		`<InstanceID>0</InstanceID><Channel>Master</Channel>` + extraXML +
		`</u:` + action + `></s:Body></s:Envelope>`
	soap := `SOAPACTION: "` + upnpNS + `#` + action + `"`
	return fmt.Sprintf("curl -sS -m %d -X POST %s -H %s -H %s --data-binary %s",
		curlMax,
		shellSingle(url),
		shellSingle(`Content-Type: text/xml; charset="utf-8"`),
		shellSingle(soap),
		shellSingle(body),
	), nil
}

func curlVolumeGet(host string) (string, error) {
	return curlUPNP(host, "GetVolume", "")
}

func curlVolumeSet(host string, level int) (string, error) {
	if level < 0 || level > 100 {
		return "", fmt.Errorf("volume level must be 0–100")
	}
	set, err := curlUPNP(host, "SetVolume", fmt.Sprintf("<DesiredVolume>%d</DesiredVolume>", level))
	if err != nil {
		return "", err
	}
	get, err := curlVolumeGet(host)
	if err != nil {
		return "", err
	}
	return set + " >/dev/null && " + get, nil
}

func curlMuteGet(host string) (string, error) {
	return curlUPNP(host, "GetMute", "")
}

func curlMuteSet(host string, muted bool) (string, error) {
	v := "0"
	if muted {
		v = "1"
	}
	set, err := curlUPNP(host, "SetMute", "<DesiredMute>"+v+"</DesiredMute>")
	if err != nil {
		return "", err
	}
	get, err := curlMuteGet(host)
	if err != nil {
		return "", err
	}
	return set + " >/dev/null && " + get, nil
}

func curlMuteToggle(host string) (string, error) {
	get, err := curlMuteGet(host)
	if err != nil {
		return "", err
	}
	set0, err := curlMuteSet(host, false)
	if err != nil {
		return "", err
	}
	set1, err := curlMuteSet(host, true)
	if err != nil {
		return "", err
	}
	return "xml=$(" + get + ") && " +
		`cur=$(printf '%s' "$xml" | sed -n 's/.*<CurrentMute>\([^<]*\)<\/CurrentMute>.*/\1/p') && ` +
		`case "$cur" in 1|true|True|TRUE) ` + set0 + ` ;; *) ` + set1 + ` ;; esac`, nil
}

// parseSOAPTag reads the first <Tag>value</Tag> (namespace prefix allowed).
func parseSOAPTag(xml, tag string) string {
	re := regexp.MustCompile(`(?i)<(?:[A-Za-z0-9._-]+:)?` + regexp.QuoteMeta(tag) + `>([^<]*)</(?:[A-Za-z0-9._-]+:)?` + regexp.QuoteMeta(tag) + `>`)
	m := re.FindStringSubmatch(xml)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func parseVolume(xml string) (int, bool) {
	s := parseSOAPTag(xml, "CurrentVolume")
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func parseMute(xml string) (bool, bool) {
	s := strings.ToLower(parseSOAPTag(xml, "CurrentMute"))
	switch s {
	case "1", "true", "yes":
		return true, true
	case "0", "false", "no":
		return false, true
	default:
		return false, false
	}
}

func wolCmd(mac, host string) (string, error) {
	mac = strings.TrimSpace(mac)
	if err := validMAC(mac); err != nil {
		return "", err
	}
	bcast := "255.255.255.255"
	if err := validHost(host); err == nil {
		if ip := parseIPv4(host); ip != "" {
			parts := strings.Split(ip, ".")
			if len(parts) == 4 {
				bcast = parts[0] + "." + parts[1] + "." + parts[2] + ".255"
			}
		}
	}
	hex := strings.ReplaceAll(mac, ":", "")
	body := "import socket\n" +
		"m=bytes.fromhex(" + pyStr(hex) + ")\n" +
		"p=b'\\xff'*6+m*16\n" +
		"s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM)\n" +
		"s.setsockopt(socket.SOL_SOCKET,socket.SO_BROADCAST,1)\n" +
		"s.sendto(p,('255.255.255.255',9))\n" +
		"s.sendto(p,(" + pyStr(bcast) + ",9))\n" +
		"print('wol sent')\n"
	return "python3 -c " + shellSingle(body) + "; sleep 2", nil
}

func parseIPv4(host string) string {
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return ""
	}
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return ""
		}
	}
	return host
}
