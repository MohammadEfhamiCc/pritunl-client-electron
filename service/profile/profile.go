package profile

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dropbox/godropbox/container/set"
	"github.com/dropbox/godropbox/errors"
	"github.com/pritunl/pritunl-client-electron/service/command"
	"github.com/pritunl/pritunl-client-electron/service/errortypes"
	"github.com/pritunl/pritunl-client-electron/service/event"
	"github.com/pritunl/pritunl-client-electron/service/network"
	"github.com/pritunl/pritunl-client-electron/service/parser"
	"github.com/pritunl/pritunl-client-electron/service/platform"
	"github.com/pritunl/pritunl-client-electron/service/sprofile"
	"github.com/pritunl/pritunl-client-electron/service/token"
	"github.com/pritunl/pritunl-client-electron/service/tuntap"
	"github.com/pritunl/pritunl-client-electron/service/utils"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/nacl/box"
)

const (
	connTimeout  = 60 * time.Second
	resetWait    = 3000 * time.Millisecond
	netResetWait = 4000 * time.Millisecond
)

var (
	shutdown = false
	Profiles = struct {
		sync.RWMutex
		m map[string]*Profile
	}{
		m: map[string]*Profile{},
	}
	Ping            = time.Now()
	clientTransport = &http.Transport{
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 5 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
			MaxVersion:         tls.VersionTLS13,
		},
	}
	clientInsecure = &http.Client{
		Transport: clientTransport,
		Timeout:   6 * time.Second,
	}
	clientConnInsecure = &http.Client{
		Transport: clientTransport,
		Timeout:   30 * time.Second,
	}
	ipReg      = regexp.MustCompile(`(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)(\.(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)){3}`)
	profileReg = regexp.MustCompile(`[^a-z0-9_\- ]+`)
)

type WgKeyReq struct {
	Data      string `json:"data"`
	Nonce     string `json:"nonce"`
	PublicKey string `json:"public_key"`
	Signature string `json:"signature"`
}

type WgKeyBox struct {
	DeviceId       string   `json:"device_id"`
	DeviceName     string   `json:"device_name"`
	Platform       string   `json:"platform"`
	MacAddr        string   `json:"mac_addr"`
	MacAddrs       []string `json:"mac_addrs"`
	Token          string   `json:"token"`
	Nonce          string   `json:"nonce"`
	Password       string   `json:"password"`
	Timestamp      int64    `json:"timestamp"`
	WgPublicKey    string   `json:"wg_public_key"`
	PublicAddress  string   `json:"public_address"`
	PublicAddress6 string   `json:"public_address6"`
}

type OvpnKeyBox struct {
	DeviceId       string   `json:"device_id"`
	DeviceName     string   `json:"device_name"`
	Platform       string   `json:"platform"`
	MacAddr        string   `json:"mac_addr"`
	MacAddrs       []string `json:"mac_addrs"`
	Token          string   `json:"token"`
	Nonce          string   `json:"nonce"`
	Password       string   `json:"password"`
	Timestamp      int64    `json:"timestamp"`
	PublicAddress  string   `json:"public_address"`
	PublicAddress6 string   `json:"public_address6"`
}

type KeyResp struct {
	Data      string `json:"data"`
	Nonce     string `json:"nonce"`
	Signature string `json:"signature"`
}

type Route struct {
	NextHop    string `json:"next_hop"`
	Network    string `json:"network"`
	Metric     int    `json:"metric"`
	NetGateway bool   `json:"net_gateway"`
}

type WgConf struct {
	Address       string   `json:"address"`
	Address6      string   `json:"address6"`
	Hostname      string   `json:"hostname"`
	Hostname6     string   `json:"hostname6"`
	Gateway       string   `json:"gateway"`
	Gateway6      string   `json:"gateway6"`
	Port          int      `json:"port"`
	WebPort       int      `json:"web_port"`
	WebNoSsl      bool     `json:"web_no_ssl"`
	PublicKey     string   `json:"public_key"`
	Routes        []*Route `json:"routes"`
	Routes6       []*Route `json:"routes6"`
	DnsServers    []string `json:"dns_servers"`
	SearchDomains []string `json:"search_domains"`
}

type WgData struct {
	Allow         bool    `json:"allow"`
	Reason        string  `json:"reason"`
	Configuration *WgConf `json:"configuration"`
}

type OvpnData struct {
	Allow   bool   `json:"allow"`
	Reason  string `json:"reason"`
	Token   string `json:"token"`
	Remote  string `json:"remote"`
	Remote6 string `json:"remote6"`
}

type WgPingData struct {
	Status    bool `json:"status"`
	Timestamp int  `json:"timestamp"`
}

type OutputData struct {
	Id     string `json:"id"`
	Output string `json:"output"`
}

type Profile struct {
	state              bool               `json:"-"`
	stateLock          sync.Mutex         `json:"-"`
	wgQuickLock        sync.Mutex         `json:"-"`
	connected          bool               `json:"-"`
	stop               bool               `json:"-"`
	startTime          time.Time          `json:"-"`
	authFailed         bool               `json:"-"`
	waiters            []chan bool        `json:"-"`
	remPaths           []string           `json:"-"`
	bashPath           string             `json:"-"`
	wgPath             string             `json:"-"`
	wgQuickPath        string             `json:"-"`
	wgConfPth          string             `json:"-"`
	wgHandshake        int                `json:"-"`
	wgServerPublicKey  string             `json:"-"`
	openReqCancel      context.CancelFunc `json:"-"`
	cmd                *exec.Cmd          `json:"-"`
	tap                string             `json:"-"`
	lastAuthErr        time.Time          `json:"-"`
	token              *token.Token       `json:"-"`
	managementPass     string             `json:"-"`
	managementPort     int                `json:"-"`
	Id                 string             `json:"id"`
	Mode               string             `json:"mode"`
	OrgId              string             `json:"-"`
	UserId             string             `json:"-"`
	ServerId           string             `json:"-"`
	SyncHosts          []string           `json:"-"`
	SyncToken          string             `json:"-"`
	SyncSecret         string             `json:"-"`
	PrivateKeyWg       string             `json:"-"`
	PublicKeyWg        string             `json:"-"`
	PrivateKey         string             `json:"-"`
	DeviceId           string             `json:"-"`
	DeviceName         string             `json:"-"`
	Data               string             `json:"-"`
	Username           string             `json:"-"`
	Password           string             `json:"-"`
	DynamicFirewall    bool               `json:"-"`
	ServerPublicKey    string             `json:"-"`
	ServerBoxPublicKey string             `json:"-"`
	TokenTtl           int                `json:"-"`
	Iface              string             `json:"iface"`
	Tuniface           string             `json:"tun_iface"`
	Routes             []*Route           `json:"routes'"`
	Routes6            []*Route           `json:"routes6'"`
	Reconnect          bool               `json:"reconnect"`
	Status             string             `json:"status"`
	Timestamp          int64              `json:"timestamp"`
	GatewayAddr        string             `json:"gateway_addr"`
	GatewayAddr6       string             `json:"gateway_addr6"`
	ServerAddr         string             `json:"server_addr"`
	ClientAddr         string             `json:"client_addr"`
	MacAddr            string             `json:"mac_addr"`
	MacAddrs           []string           `json:"mac_addrs"`
	WebPort            int                `json:"web_port"`
	WebNoSsl           bool               `json:"web_no_ssl"`
	SystemProfile      *sprofile.Sprofile `json:"-"`
}

type AuthData struct {
	Token     string `json:"token"`
	Password  string `json:"password"`
	Nonce     string `json:"nonce"`
	Timestamp int64  `json:"timestamp"`
}

func (p *Profile) write(fixedRemote, fixedRemote6 string) (
	pth string, err error) {

	rootDir, err := utils.GetTempDir()
	if err != nil {
		return
	}

	pth = filepath.Join(rootDir, p.Id)

	prsr := parser.Import(p.Data, fixedRemote, fixedRemote6)
	data := prsr.Export()

	if runtime.GOOS == "windows" {
		p.managementPort = ManagementPortAcquire()

		managementPassPath, e := p.writeManagementPass()
		if e != nil {
			err = e
			return
		}
		p.remPaths = append(p.remPaths, managementPassPath)

		data += fmt.Sprintf(
			"management 127.0.0.1 %d %s\n",
			p.managementPort,
			strings.ReplaceAll(managementPassPath, "\\", "\\\\"),
		)
	}

	_ = os.Remove(pth)
	err = ioutil.WriteFile(pth, []byte(data), os.FileMode(0600))
	if err != nil {
		err = &WriteError{
			errors.Wrap(err, "profile: Failed to write profile"),
		}
		return
	}

	return
}

func (p *Profile) writeUp() (pth string, err error) {
	rootDir, err := utils.GetTempDir()
	if err != nil {
		return
	}

	if runtime.GOOS == "windows" {
		pth = filepath.Join(rootDir, p.Id+"-up.bat")
	} else {
		pth = filepath.Join(rootDir, p.Id+"-up.sh")
	}

	script := ""
	switch runtime.GOOS {
	case "darwin":
		script = upScriptDarwin
		break
	case "linux":
		resolved := true

		resolvData, _ := ioutil.ReadFile("/etc/resolv.conf")
		if resolvData != nil {
			resolvDataStr := string(resolvData)
			if !strings.Contains(resolvDataStr, "systemd-resolved") &&
				!strings.Contains(resolvDataStr, "127.0.0.53") {

				resolved = false
			}
		}

		if resolved {
			script = resolvedScript
		} else {
			script = resolvScript
		}
		break
	default:
		panic("profile: Not implemented")
	}

	_ = os.Remove(pth)
	err = ioutil.WriteFile(pth, []byte(script), os.FileMode(0755))
	if err != nil {
		err = &WriteError{
			errors.Wrap(err, "profile: Failed to write up script"),
		}
		return
	}

	return
}

func (p *Profile) writeDown() (pth string, err error) {
	rootDir, err := utils.GetTempDir()
	if err != nil {
		return
	}

	if runtime.GOOS == "windows" {
		pth = filepath.Join(rootDir, p.Id+"-down.bat")
	} else {
		pth = filepath.Join(rootDir, p.Id+"-down.sh")
	}

	script := ""
	switch runtime.GOOS {
	case "darwin":
		script = downScriptDarwin
		break
	case "linux":
		resolved := true

		resolvData, _ := ioutil.ReadFile("/etc/resolv.conf")
		if resolvData != nil {
			resolvDataStr := string(resolvData)
			if !strings.Contains(resolvDataStr, "systemd-resolved") &&
				!strings.Contains(resolvDataStr, "127.0.0.53") {

				resolved = false
			}
		}

		if resolved {
			script = resolvedScript
		} else {
			script = resolvScript
		}
		break
	default:
		panic("profile: Not implemented")
	}

	_ = os.Remove(pth)
	err = ioutil.WriteFile(pth, []byte(script), os.FileMode(0755))
	if err != nil {
		err = &WriteError{
			errors.Wrap(err, "profile: Failed to write down script"),
		}
		return
	}

	return
}

func (p *Profile) writeBlock() (pth string, err error) {
	rootDir, err := utils.GetTempDir()
	if err != nil {
		return
	}

	script := ""
	if runtime.GOOS == "windows" {
		pth = filepath.Join(rootDir, p.Id+"-block.bat")
		script = blockScriptWindows
	} else {
		pth = filepath.Join(rootDir, p.Id+"-block.sh")
		script = blockScript
	}

	_ = os.Remove(pth)
	err = ioutil.WriteFile(pth, []byte(script), os.FileMode(0755))
	if err != nil {
		err = &WriteError{
			errors.Wrap(err, "profile: Failed to write block script"),
		}
		return
	}

	return
}

func (p *Profile) writeManagementPass() (pth string, err error) {
	rootDir, err := utils.GetTempDir()
	if err != nil {
		return
	}

	p.managementPass, err = utils.RandStr(32)
	if err != nil {
		return
	}

	if runtime.GOOS == "windows" {
		pth = filepath.Join(rootDir, p.Id+"-management.txt")
	} else {
		pth = filepath.Join(rootDir, p.Id+"-management")
	}

	_ = os.Remove(pth)
	err = ioutil.WriteFile(pth, []byte(p.managementPass),
		os.FileMode(0600))
	if err != nil {
		err = &WriteError{
			errors.Wrap(err, "profile: Failed to write management"),
		}
		return
	}

	return
}

func (p *Profile) writeAuth(fwToken string) (pth string, err error) {
	rootDir, err := utils.GetTempDir()
	if err != nil {
		return
	}

	username := p.Username
	password := p.Password

	if fwToken != "" {
		var serverPubKey [32]byte
		serverPubKeySlic, e := base64.StdEncoding.DecodeString(
			p.ServerBoxPublicKey)
		if e != nil {
			err = &errortypes.ParseError{
				errors.Wrap(e, "profile: Failed to decode server box key"),
			}
			return
		}
		copy(serverPubKey[:], serverPubKeySlic)

		senderPubKey, senderPrivKey, e := box.GenerateKey(rand.Reader)
		if e != nil {
			err = &errortypes.ReadError{
				errors.Wrap(e, "profile: Failed to generate nacl key"),
			}
			return
		}

		var nonce [24]byte
		nonceHash := sha256.Sum256(senderPubKey[:])
		copy(nonce[:], nonceHash[:24])

		username = base64.RawStdEncoding.EncodeToString(senderPubKey[:])

		encrypted := box.Seal([]byte{}, []byte(fwToken),
			&nonce, &serverPubKey, senderPrivKey)

		ciphertext64 := base64.RawStdEncoding.EncodeToString(encrypted)
		password = "$f$" + ciphertext64
	} else if p.ServerBoxPublicKey != "" {
		var serverPubKey [32]byte
		serverPubKeySlic, e := base64.StdEncoding.DecodeString(
			p.ServerBoxPublicKey)
		if e != nil {
			err = &errortypes.ParseError{
				errors.Wrap(e, "profile: Failed to decode server box key"),
			}
			return
		}
		copy(serverPubKey[:], serverPubKeySlic)

		tokn := token.Get(p.Id, p.ServerPublicKey, p.ServerBoxPublicKey)
		p.token = tokn

		authToken := ""
		if tokn != nil {
			err = tokn.Update()
			if err != nil {
				return
			}

			authToken = tokn.Token
		} else {
			authToken, err = utils.RandStrComplex(16)
			if err != nil {
				return
			}
		}

		authData := strings.Join([]string{
			authToken,
			fmt.Sprintf("%d", time.Now().Unix()),
			password,
		}, "")

		senderPubKey, senderPrivKey, e := box.GenerateKey(rand.Reader)
		if e != nil {
			err = &errortypes.ReadError{
				errors.Wrap(e, "profile: Failed to generate nacl key"),
			}
			return
		}

		var nonce [24]byte
		nonceHash := sha256.Sum256(senderPubKey[:])
		copy(nonce[:], nonceHash[:24])

		username = base64.RawStdEncoding.EncodeToString(senderPubKey[:])

		encrypted := box.Seal([]byte{}, []byte(authData),
			&nonce, &serverPubKey, senderPrivKey)

		ciphertext64 := base64.RawStdEncoding.EncodeToString(encrypted)
		password = "$x$" + ciphertext64
	} else if p.ServerPublicKey != "" {
		block, _ := pem.Decode([]byte(p.ServerPublicKey))

		pub, e := x509.ParsePKCS1PublicKey(block.Bytes)
		if e != nil {
			err = &errortypes.ParseError{
				errors.Wrap(e, "profile: Failed to parse public key"),
			}
			return
		}

		nonce, e := utils.RandStr(32)
		if e != nil {
			err = e
			return
		}

		tokn := token.Get(p.Id, p.ServerPublicKey, p.ServerBoxPublicKey)
		p.token = tokn

		authToken := ""
		if tokn != nil {
			err = tokn.Update()
			if err != nil {
				return
			}

			authToken = tokn.Token
		}

		authData := &AuthData{
			Token:     authToken,
			Password:  password,
			Nonce:     nonce,
			Timestamp: time.Now().Unix(),
		}

		authDataJson, e := json.Marshal(authData)
		if e != nil {
			err = &errortypes.ParseError{
				errors.Wrap(e, "profile: Failed to encode auth data"),
			}
			return
		}

		ciphertext, e := rsa.EncryptOAEP(
			sha512.New(),
			rand.Reader,
			pub,
			authDataJson,
			[]byte{},
		)
		if e != nil {
			err = &errortypes.WriteError{
				errors.Wrap(e, "profile: Failed to encrypt auth data"),
			}
			return
		}

		ciphertext64 := base64.StdEncoding.EncodeToString(ciphertext)

		password = "<%=RSA_ENCRYPTED=%>" + ciphertext64
	}

	pth = filepath.Join(rootDir, p.Id+".auth")

	_ = os.Remove(pth)
	err = ioutil.WriteFile(pth, []byte(username+"\n"+password+"\n"),
		os.FileMode(0600))
	if err != nil {
		err = &WriteError{
			errors.Wrap(err, "profile: Failed to write profile auth"),
		}
		return
	}

	return
}

func (p *Profile) generateWgKey() (err error) {
	privateKey, err := utils.ExecOutput(p.wgPath, "genkey")
	if err != nil {
		err = &ExecError{
			errors.Wrap(err, "profile: Failed to generate private key"),
		}
		return
	}

	publicKey, err := utils.ExecInputOutput(privateKey, p.wgPath, "pubkey")
	if err != nil {
		err = &ExecError{
			errors.Wrap(err, "profile: Failed to get public key"),
		}
		return
	}

	p.PrivateKeyWg = strings.TrimSpace(privateKey)
	p.PublicKeyWg = strings.TrimSpace(publicKey)

	return
}

func (p *Profile) writeConfWgLinux() (pth string, err error) {
	rootDir, err := utils.GetTempDir()
	if err != nil {
		return
	}

	pth = filepath.Join(rootDir, p.Id+".key")

	_ = os.Remove(pth)
	err = ioutil.WriteFile(
		pth,
		[]byte(p.PrivateKeyWg+"\n"),
		os.FileMode(0600),
	)
	if err != nil {
		err = &WriteError{
			errors.Wrap(err, "profile: Failed to write private key"),
		}
		return
	}

	return
}

func (p *Profile) writeConfWgQuick(data *WgConf) (pth string, err error) {
	allowedIps := []string{}
	if data.Routes != nil {
		for _, route := range data.Routes {
			allowedIps = append(allowedIps, route.Network)
		}
	}
	if data.Routes6 != nil {
		for _, route := range data.Routes6 {
			allowedIps = append(allowedIps, route.Network)
		}
	}

	addr := data.Address
	if data.Address6 != "" {
		addr += "," + data.Address6
	}

	templData := WgConfData{
		Address:    addr,
		PrivateKey: p.PrivateKeyWg,
		PublicKey:  data.PublicKey,
		AllowedIps: strings.Join(allowedIps, ","),
		Endpoint:   fmt.Sprintf("%s:%d", data.Hostname, data.Port),
	}

	if data.DnsServers != nil && len(data.DnsServers) > 0 {
		templData.HasDns = true
		templData.DnsServers = strings.Join(data.DnsServers, ",")
	}

	output := &bytes.Buffer{}
	err = WgConfTempl.Execute(output, templData)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to exec wg template"),
		}
		return
	}

	rootDir := ""
	switch runtime.GOOS {
	case "linux":
		rootDir = WgLinuxConfPath

		err = platform.MkdirSecure(WgLinuxConfPath)
		if err != nil {
			return
		}
	case "darwin":
		rootDir = WgMacConfPath

		err = platform.MkdirSecure(WgMacConfPath)
		if err != nil {
			return
		}
	default:
		rootDir, err = utils.GetTempDir()
		if err != nil {
			return
		}
	}

	pth = filepath.Join(rootDir, p.Iface+".conf")

	_ = os.Remove(pth)
	err = ioutil.WriteFile(
		pth,
		[]byte(output.String()),
		os.FileMode(0600),
	)
	if err != nil {
		err = &WriteError{
			errors.Wrap(err, "profile: Failed to write wg conf"),
		}
		return
	}

	return
}

func (p *Profile) writeWgConf(data *WgConf) (pth string, err error) {
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
		pth, err = p.writeConfWgQuick(data)
		break
	default:
		panic("profile: Not implemented")
	}
	if err != nil {
		return
	}

	return
}

func (p *Profile) update() {
	evt := event.Event{
		Type: "update",
		Data: p,
	}
	evt.Init()

	status := GetStatus()

	if status {
		evt := event.Event{
			Type: "connected",
		}
		evt.Init()
	} else {
		evt := event.Event{
			Type: "disconnected",
		}
		evt.Init()
	}
}

func (p *Profile) pushOutput(output string) {
	if p.SystemProfile != nil {
		err := p.SystemProfile.PushOutput(output + "\n")
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"output": output,
				"error":  err,
			}).Error("profile: Failed to push profile log output")
		}
	} else {
		evt := &event.Event{
			Type: "output",
			Data: &OutputData{
				Id:     p.Id,
				Output: output,
			},
		}
		evt.Init()
	}

	return
}

func (p *Profile) parseLine(line string) {
	p.pushOutput(line)

	if strings.Contains(line, "Initialization Sequence Completed") {
		p.connected = true
		p.Status = "connected"
		p.Timestamp = time.Now().Unix() - 5
		p.update()

		tokn := p.token
		if tokn != nil {
			tokn.Valid = true
		}

		go func() {
			defer func() {
				panc := recover()
				if panc != nil {
					logrus.WithFields(logrus.Fields{
						"stack": string(debug.Stack()),
						"panic": panc,
					}).Error("profile: Panic")
					panic(panc)
				}
			}()

			utils.ClearDNSCache()
		}()
	} else if strings.Contains(line, "Inactivity timeout (--inactive)") {
		evt := event.Event{
			Type: "inactive",
			Data: p,
		}
		evt.Init()

		p.stop = true
	} else if strings.Contains(line, "Inactivity timeout") ||
		strings.Contains(line, "Connection reset") {

		go func() {
			defer func() {
				panc := recover()
				if panc != nil {
					logrus.WithFields(logrus.Fields{
						"stack": string(debug.Stack()),
						"panic": panc,
					}).Error("profile: Panic")
					panic(panc)
				}
			}()

			restartLock.Lock()
			if p.stop {
				restartLock.Unlock()
				return
			}

			prfl := p.Copy()
			restartLock.Unlock()

			err := p.Stop()
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"error": err,
				}).Error("profile: Stop error")
				return
			}

			p.Wait()

			if prfl.Reconnect {
				err = prfl.Start(false, true)
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"error": err,
					}).Error("profile: Restart error")
					return
				}
			}
		}()
	} else if strings.Contains(
		line, "Can't assign requested address (code=49)") {

		go func() {
			defer func() {
				panc := recover()
				if panc != nil {
					logrus.WithFields(logrus.Fields{
						"stack": string(debug.Stack()),
						"panic": panc,
					}).Error("profile: Panic")
					panic(panc)
				}
			}()

			time.Sleep(3 * time.Second)

			if !p.stop {
				RestartProfiles(true)
			}
		}()
	} else if strings.Contains(line, "AUTH_FAILED") || strings.Contains(
		line, "auth-failure") && !p.authFailed {

		p.stop = true
		p.authFailed = true

		tokn := p.token
		if tokn != nil {
			tokn.Init()
		}

		if utils.SinceAbs(p.lastAuthErr) > 10*time.Second {
			p.lastAuthErr = time.Now()

			evt := event.Event{
				Type: "auth_error",
				Data: p,
			}
			evt.Init()

			if p.SystemProfile != nil {
				if p.SystemProfile.AuthErrorCount >= 10 {
					logrus.WithFields(logrus.Fields{
						"profile_id": p.SystemProfile.Id,
					}).Error("profile: Stopping system " +
						"profile due to authentication errors")

					p.SystemProfile.State = false
					sprofile.Deactivate(p.SystemProfile.Id)
					sprofile.SetAuthErrorCount(
						p.SystemProfile.Id,
						0,
					)
				} else {
					sprofile.SetAuthErrorCount(
						p.SystemProfile.Id,
						p.SystemProfile.AuthErrorCount+1,
					)

					time.Sleep(10 * time.Second)
				}
			} else {
				time.Sleep(3 * time.Second)
			}
		}
	} else if strings.Contains(line, "link remote:") {
		sIndex := strings.LastIndex(line, "]") + 1
		eIndex := strings.LastIndex(line, ":")

		p.ServerAddr = line[sIndex:eIndex]
		p.update()
	} else if strings.Contains(line, "network/local/netmask") {
		eIndex := strings.LastIndex(line, "/")
		line = line[:eIndex]
		sIndex := strings.LastIndex(line, "/") + 1

		p.ClientAddr = line[sIndex:]
		p.update()
	} else if strings.Contains(line, "ifconfig") && strings.Contains(
		line, "netmask") {

		sIndex := strings.Index(line, "ifconfig") + 9
		eIndex := strings.Index(line, "netmask")
		line = line[sIndex:eIndex]

		split := strings.Split(line, " ")
		if len(split) > 2 {
			p.ClientAddr = split[1]
			p.update()
		}
	} else if strings.Contains(line, "ip addr add dev") {
		clientAddr := ""
		sIndex := strings.Index(line, "ip addr add dev") + 16
		eIndex := strings.Index(line, "broadcast")

		if eIndex == -1 {
			ipList := ipReg.FindAllString(line, -1)
			if len(ipList) > 0 {
				clientAddr = ipList[0]
			}
		} else {
			line = line[sIndex:eIndex]
			split := strings.Split(line, " ")

			if len(split) > 1 {
				split := strings.Split(split[1], "/")
				if len(split) > 1 {
					clientAddr = split[0]
				}
			}
		}

		if clientAddr != "" {
			p.ClientAddr = clientAddr
			p.update()
		}
	} else if strings.Contains(line, "net_addr_v4_add:") {
		clientAddr := ""
		line = line[strings.Index(line, "net_addr_v4_add:")+17:]
		line = strings.TrimSpace(line)

		ipList := ipReg.FindAllString(line, -1)
		if len(ipList) > 0 {
			clientAddr = ipList[0]
		}

		if clientAddr != "" {
			p.ClientAddr = clientAddr
			p.update()
		}
	}
}

func (p *Profile) clearWgLinux() {
	if p.Iface != "" {
		p.wgQuickLock.Lock()
		utils.ExecCombinedOutputLogged(
			[]string{
				"does not exist",
				"is not a",
			},
			p.wgQuickPath,
			"down", p.Iface,
		)
		p.wgQuickLock.Unlock()
		network.InterfaceRelease(p.Iface)
	}
}

func (p *Profile) clearWgMac() {
	if p.Iface != "" {
		p.wgQuickLock.Lock()
		utils.ExecCombinedOutputLogged(
			[]string{
				"is not a",
			},
			p.bashPath,
			p.wgQuickPath,
			"down", p.Iface,
		)
		p.wgQuickLock.Unlock()
		network.InterfaceRelease(p.Iface)
	}
}

func (p *Profile) clearWgWin() {
	if p.Iface != "" {
		p.wgQuickLock.Lock()
		_, _ = utils.ExecCombinedOutput(
			"sc.exe", "stop", fmt.Sprintf("WireGuardTunnel$%s", p.Iface),
		)
		time.Sleep(100 * time.Millisecond)
		_, _ = utils.ExecCombinedOutput(
			"sc.exe", "delete", fmt.Sprintf("WireGuardTunnel$%s", p.Iface),
		)
		network.InterfaceRelease(p.Iface)
		p.wgQuickLock.Unlock()
	}
}

func (p *Profile) clearWg() {
	switch runtime.GOOS {
	case "linux":
		p.clearWgLinux()
		break
	case "darwin":
		p.clearWgMac()
		break
	case "windows":
		p.clearWgWin()
		break
	}

	return
}

func (p *Profile) clearOvpn() {
	if p.cmd != nil {
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Process.Kill()
		time.Sleep(1 * time.Second)
	}

	return
}

func (p *Profile) clearStatus(start time.Time) {
	if p.tap != "" {
		tuntap.Release(p.tap)
	}

	if p.managementPort != 0 {
		ManagementPortRelease(p.managementPort)
	}

	go func() {
		defer func() {
			panc := recover()
			if panc != nil {
				logrus.WithFields(logrus.Fields{
					"stack": string(debug.Stack()),
					"panic": panc,
				}).Error("profile: Panic")
				panic(panc)
			}
		}()

		diff := utils.SinceAbs(start)
		if diff < 1*time.Second {
			time.Sleep(1 * time.Second)
		}

		p.clearWg()
		p.clearOvpn()

		p.Status = "disconnected"
		p.Timestamp = 0
		p.ClientAddr = ""
		p.ServerAddr = ""
		p.update()

		for _, path := range p.remPaths {
			os.Remove(path)
		}

		Profiles.Lock()
		prfl := Profiles.m[p.Id]
		if prfl == p {
			delete(Profiles.m, p.Id)
		}
		if runtime.GOOS == "darwin" && len(Profiles.m) == 0 {
			err := utils.ClearScutilKeys()
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"error": err,
				}).Error("profile: Failed to clear scutil keys")
			}
		}
		Profiles.Unlock()

		p.stateLock.Lock()
		p.state = false
		for _, waiter := range p.waiters {
			waiter <- true
		}
		p.waiters = []chan bool{}
		p.stateLock.Unlock()

		logrus.WithFields(logrus.Fields{
			"profile_id": p.Id,
		}).Info("profile: Disconnected")
	}()
}

func (p *Profile) Copy() (prfl *Profile) {
	prfl = &Profile{
		Id:                 p.Id,
		Mode:               p.Mode,
		OrgId:              p.OrgId,
		UserId:             p.UserId,
		ServerId:           p.ServerId,
		SyncHosts:          p.SyncHosts,
		SyncToken:          p.SyncToken,
		SyncSecret:         p.SyncSecret,
		Data:               p.Data,
		Username:           p.Username,
		Password:           p.Password,
		DynamicFirewall:    p.DynamicFirewall,
		ServerPublicKey:    p.ServerPublicKey,
		ServerBoxPublicKey: p.ServerBoxPublicKey,
		Reconnect:          p.Reconnect,
		SystemProfile:      p.SystemProfile,
		connected:          p.connected,
	}
	prfl.Init()

	return
}

func (p *Profile) Init() {
	p.Id = utils.FilterStr(p.Id)
	p.stateLock = sync.Mutex{}
	p.waiters = []chan bool{}
	p.bashPath = GetBashPath()
	p.wgPath = GetWgPath()
	p.wgQuickPath = GetWgQuickPath()
}

func (p *Profile) Start(timeout bool, delay bool) (err error) {
	if shutdown {
		return
	}

	start := time.Now()
	p.startTime = start
	p.remPaths = []string{}

	p.Status = "connecting"
	p.stateLock.Lock()
	p.state = true
	p.stateLock.Unlock()

	Profiles.RLock()
	n := len(Profiles.m)
	_, ok := Profiles.m[p.Id]
	Profiles.RUnlock()
	if ok {
		return
	}

	logrus.WithFields(logrus.Fields{
		"profile_id":       p.Id,
		"mode":             p.Mode,
		"dynamic_firewall": p.DynamicFirewall,
	}).Info("profile: Connecting")

	if runtime.GOOS == "darwin" && n == 0 {
		utils.ClearScutilKeys()
	}

	Profiles.Lock()
	prfl := Profiles.m[p.Id]
	if prfl != nil {
		go func() {
			_ = prfl.Stop()
		}()
	}
	Profiles.m[p.Id] = p
	Profiles.Unlock()

	if p.SystemProfile != nil {
		updated, e := p.SystemProfile.Sync()
		if e != nil {
			logrus.WithFields(logrus.Fields{
				"profile_id": p.Id,
				"error":      e,
			}).Error("profile: Failed to sync system profile")
		} else if updated {
			UpdateSystemProfile(p, p.SystemProfile)
		}
	}

	if delay {
		time.Sleep(3 * time.Second)
		if p.stop {
			return
		}
	}

	if p.Mode == Wg {
		err = p.startWg(timeout)
	} else {
		err = p.startOvpn(timeout)
	}

	if p.stop {
		err = nil
	}

	return
}

func (p *Profile) startOvpn(timeout bool) (err error) {
	var data *OvpnData

	if p.DynamicFirewall {
		data, err = p.openOvpn()
		if err != nil {
			return
		}

		if data != nil && !data.Allow {
			logrus.WithFields(logrus.Fields{
				"reason": data.Reason,
			}).Error("profile: Failed to authenticate ovpn")

			evt := event.Event{
				Type: "auth_error",
				Data: p,
			}
			evt.Init()

			p.clearStatus(p.startTime)
			return
		}
	}

	if runtime.GOOS == "windows" {
		Profiles.Lock()
		n := len(Profiles.m)
		Profiles.Unlock()

		err = tuntap.Resize(n)
		if err != nil {
			return
		}
	}

	fixedRemote := ""
	fixedRemote6 := ""
	fwToken := ""
	if data != nil {
		fixedRemote = data.Remote
		fixedRemote6 = data.Remote6
		fwToken = data.Token
	}

	confPath, err := p.write(fixedRemote, fixedRemote6)
	if err != nil {
		p.clearStatus(p.startTime)
		return
	}
	p.remPaths = append(p.remPaths, confPath)

	var authPath string
	if (p.Username != "" && p.Password != "") ||
		p.ServerBoxPublicKey != "" || p.ServerPublicKey != "" {

		authPath, err = p.writeAuth(fwToken)
		if err != nil {
			p.clearStatus(p.startTime)
			return
		}
		p.remPaths = append(p.remPaths, authPath)
	}

	p.update()

	args := []string{
		"--config", confPath,
		"--verb", "2",
	}

	if runtime.GOOS == "windows" {
		p.tap = tuntap.Acquire()

		if p.tap != "" {
			args = append(args, "--dev-node", p.tap)
		} else {
			logrus.WithFields(logrus.Fields{
				"tap_size": tuntap.Size(),
			}).Error("profile: Failed to acquire tap")
		}
	}

	blockPath, e := p.writeBlock()
	if e != nil {
		err = e
		p.clearStatus(p.startTime)
		return
	}
	p.remPaths = append(p.remPaths, blockPath)

	switch runtime.GOOS {
	case "windows":
		args = append(args, "--script-security", "1")
		break
	case "darwin":
		upPath, e := p.writeUp()
		if e != nil {
			err = e
			p.clearStatus(p.startTime)
			return
		}
		p.remPaths = append(p.remPaths, upPath)

		downPath, e := p.writeDown()
		if e != nil {
			err = e
			p.clearStatus(p.startTime)
			return
		}
		p.remPaths = append(p.remPaths, downPath)

		args = append(args, "--script-security", "2",
			"--up", upPath,
			"--down", downPath,
			"--route-pre-down", blockPath,
			"--tls-verify", blockPath,
			"--ipchange", blockPath,
			"--route-up", blockPath,
		)
		break
	case "linux":
		upPath, e := p.writeUp()
		if e != nil {
			err = e
			p.clearStatus(p.startTime)
			return
		}
		p.remPaths = append(p.remPaths, upPath)

		downPath, e := p.writeDown()
		if e != nil {
			err = e
			p.clearStatus(p.startTime)
			return
		}
		p.remPaths = append(p.remPaths, downPath)

		args = append(args, "--script-security", "2",
			"--up", upPath,
			"--down", downPath,
			"--route-pre-down", blockPath,
			"--tls-verify", blockPath,
			"--ipchange", blockPath,
			"--route-up", blockPath,
		)
		break
	default:
		panic("profile: Not implemented")
	}

	if authPath != "" {
		args = append(args, "--auth-user-pass", authPath)
	}

	cmd := command.Command(getOpenvpnPath(), args...)
	cmd.Dir = getOpenvpnDir()
	p.cmd = cmd

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		err = &ExecError{
			errors.Wrap(err, "profile: Failed to get stdout"),
		}
		p.clearStatus(p.startTime)
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		err = &ExecError{
			errors.Wrap(err, "profile: Failed to get stderr"),
		}
		p.clearStatus(p.startTime)
		return
	}

	if p.stop {
		p.clearStatus(p.startTime)
		return
	}

	output := make(chan string, 100)
	outputWait := sync.WaitGroup{}
	outputWait.Add(1)

	go func() {
		defer func() {
			panc := recover()
			if panc != nil {
				logrus.WithFields(logrus.Fields{
					"stack": string(debug.Stack()),
					"panic": panc,
				}).Error("profile: Panic")
				panic(panc)
			}
		}()

		defer func() {
			_ = stdout.Close()
			output <- ""
		}()

		out := bufio.NewReader(stdout)
		for {
			line, _, err := out.ReadLine()
			if err != nil {
				if err != io.EOF &&
					!strings.Contains(err.Error(), "file already closed") &&
					!strings.Contains(err.Error(), "bad file descriptor") {

					err = &errortypes.ReadError{
						errors.Wrap(err, "profile: Failed to read stdout"),
					}
					logrus.WithFields(logrus.Fields{
						"error": err,
					}).Error("profile: Stdout error")
				}

				return
			}

			lineStr := string(line)
			if lineStr != "" {
				output <- lineStr
			}
		}
	}()

	go func() {
		defer func() {
			panc := recover()
			if panc != nil {
				logrus.WithFields(logrus.Fields{
					"stack": string(debug.Stack()),
					"panic": panc,
				}).Error("profile: Panic")
				panic(panc)
			}
		}()

		defer stderr.Close()

		out := bufio.NewReader(stderr)
		for {
			line, _, err := out.ReadLine()
			if err != nil {
				if err != io.EOF &&
					!strings.Contains(err.Error(), "file already closed") &&
					!strings.Contains(err.Error(), "bad file descriptor") {

					err = &errortypes.ReadError{
						errors.Wrap(err, "profile: Failed to read stderr"),
					}
					logrus.WithFields(logrus.Fields{
						"error": err,
					}).Error("profile: Stderr error")
				}

				return
			}

			lineStr := string(line)
			if lineStr != "" {
				output <- lineStr
			}
		}
	}()

	go func() {
		defer func() {
			panc := recover()
			if panc != nil {
				logrus.WithFields(logrus.Fields{
					"stack": string(debug.Stack()),
					"panic": panc,
				}).Error("profile: Panic")
				panic(panc)
			}
		}()

		defer outputWait.Done()

		for {
			line := <-output
			if line == "" {
				return
			}

			p.parseLine(line)
		}
	}()

	err = cmd.Start()
	if err != nil {
		err = &ExecError{
			errors.Wrap(err, "profile: Failed to start openvpn"),
		}
		p.clearStatus(p.startTime)
		return
	}

	running := true
	go func() {
		defer func() {
			panc := recover()
			if panc != nil {
				logrus.WithFields(logrus.Fields{
					"stack": string(debug.Stack()),
					"panic": panc,
				}).Error("profile: Panic")
				panic(panc)
			}
		}()

		cmd.Wait()
		outputWait.Wait()
		running = false

		if runtime.GOOS == "darwin" {
			err = utils.RestoreScutilDns()
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"error": err,
				}).Error("profile: Failed to restore DNS")
			}
		}

		if !p.stop {
			logrus.WithFields(logrus.Fields{
				"profile_id": p.Id,
			}).Info("profile: Profile exit, reconnecting")
		}

		p.clearStatus(p.startTime)
	}()

	if timeout {
		go func() {
			defer func() {
				panc := recover()
				if panc != nil {
					logrus.WithFields(logrus.Fields{
						"stack": string(debug.Stack()),
						"panic": panc,
					}).Error("profile: Panic")
					panic(panc)
				}
			}()

			time.Sleep(connTimeout)
			if p.Status != "connected" && running {
				if runtime.GOOS == "windows" {
					_ = cmd.Process.Kill()
				} else {
					err = p.cmd.Process.Signal(os.Interrupt)
					if err != nil {
						err = &ExecError{
							errors.Wrap(err,
								"profile: Failed to interrupt openvpn"),
						}
						return
					}

					done := false

					go func() {
						defer func() {
							panc := recover()
							if panc != nil {
								logrus.WithFields(logrus.Fields{
									"stack": string(debug.Stack()),
									"panic": panc,
								}).Error("profile: Panic")
								panic(panc)
							}
						}()

						time.Sleep(3 * time.Second)
						if done {
							return
						}
						_ = p.cmd.Process.Kill()
					}()

					utils.ExecWaitTimeout(p.cmd.Process, 10*time.Second)
					done = true
				}

				evt := event.Event{
					Type: "timeout_error",
					Data: p,
				}
				evt.Init()
			}
		}()
	}

	return
}

func (p *Profile) openOvpn() (data *OvpnData, err error) {
	remotesSet := set.NewSet()
	remotes := []string{}

	ifaces, err := net.Interfaces()
	if err != nil {
		err = &errortypes.ReadError{
			errors.New("profile: Failed to load interfaces"),
		}

		p.clearStatus(p.startTime)
		return
	}

	macAddr := ""
	macAddrs := []string{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 ||
			iface.Flags&net.FlagLoopback != 0 ||
			iface.HardwareAddr == nil ||
			iface.HardwareAddr.String() == "" {

			continue
		}

		macAddr = iface.HardwareAddr.String()
		if p.MacAddr == "" {
			p.MacAddr = macAddr
		}
		macAddrs = append(macAddrs, macAddr)
	}
	p.MacAddrs = macAddrs

	rangeKey := false
	for _, line := range strings.Split(p.Data, "\n") {
		if !rangeKey {
			if strings.HasPrefix(line, "setenv UV_ID") {
				lineSpl := strings.Split(line, " ")
				if len(lineSpl) < 3 {
					continue
				}

				p.DeviceId = lineSpl[2]
			} else if strings.HasPrefix(line, "setenv UV_NAME") {
				lineSpl := strings.Split(line, " ")
				if len(lineSpl) < 3 {
					continue
				}

				p.DeviceName = lineSpl[2]
			} else if strings.HasPrefix(line, "remote ") {
				lineSpl := strings.Split(line, " ")
				if len(lineSpl) < 4 {
					continue
				}

				remote := lineSpl[1]
				if !remotesSet.Contains(remote) {
					remotesSet.Add(remote)
					remotes = append(remotes, remote)
				}
			} else if strings.HasPrefix(line, "<key>") {
				rangeKey = true
			}
		} else {
			if strings.HasPrefix(line, "</key>") {
				rangeKey = false
			} else {
				p.PrivateKey += line + "\n"
			}
		}
	}

	for _, syncAddr := range p.SyncHosts {
		syncUrl, e := url.Parse(syncAddr)
		if e != nil {
			err = &errortypes.ParseError{
				errors.Wrap(e, "profile: Sync address parse error"),
			}

			logrus.WithFields(logrus.Fields{
				"error": err,
			}).Error("profile: Failed to parse sync address")

			err = nil

			continue
		}

		remote := syncUrl.Host

		if !remotesSet.Contains(remote) {
			remotesSet.Add(remote)
			remotes = append(remotes, remote)
		}
	}

	for _, i := range mathrand.Perm(len(remotes)) {
		remote := remotes[i]

		data, err = p.reqOvpn(remote)
		if err == nil {
			break
		}

		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Warn("profile: Request ovpn connection error")

		if p.stop {
			p.clearStatus(p.startTime)
			return
		}
	}
	if err != nil {
		evt := event.Event{
			Type: "connection_error",
			Data: p,
		}
		evt.Init()

		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Error("profile: Request ovpn connection failed")
		err = nil

		time.Sleep(3 * time.Second)

		p.clearStatus(p.startTime)
		if p.connected && !p.stop {
			go p.restart()
		}
		return
	}

	return
}

func (p *Profile) reqOvpn(remote string) (ovpnData *OvpnData, err error) {
	if p.ServerBoxPublicKey == "" {
		err = &errortypes.ReadError{
			errors.Wrap(err, "profile: Server box public key not set"),
		}
		return
	}

	var serverPubKey [32]byte
	serverPubKeySlic, err := base64.StdEncoding.DecodeString(
		p.ServerBoxPublicKey)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to decode server box key"),
		}
		return
	}
	copy(serverPubKey[:], serverPubKeySlic)

	tokn := token.Get(p.Id, p.ServerPublicKey, p.ServerBoxPublicKey)
	p.token = tokn

	authToken := ""
	if tokn != nil {
		err = tokn.Update()
		if err != nil {
			return
		}

		authToken = tokn.Token
	} else {
		authToken, err = utils.RandStrComplex(16)
		if err != nil {
			return
		}
	}

	tokenNonce, err := utils.RandStr(16)
	if err != nil {
		return
	}

	pltfrm := ""
	switch runtime.GOOS {
	case "linux":
		pltfrm = "linux"
		break
	case "windows":
		pltfrm = "win"
		break
	case "darwin":
		pltfrm = "mac"
		break
	default:
		pltfrm = "unknown"
		break
	}

	ovpnBox := &OvpnKeyBox{
		DeviceId:       p.DeviceId,
		DeviceName:     p.DeviceName,
		Platform:       pltfrm,
		MacAddr:        p.MacAddr,
		MacAddrs:       p.MacAddrs,
		Token:          authToken,
		Nonce:          tokenNonce,
		Password:       p.Password,
		Timestamp:      time.Now().Unix(),
		PublicAddress:  "", // TODO
		PublicAddress6: "", // TODO
	}

	ovpnBoxData, err := json.Marshal(ovpnBox)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to marshal wg key box"),
		}
		return
	}

	senderPubKey, senderPrivKey, err := box.GenerateKey(rand.Reader)
	if err != nil {
		err = &errortypes.ReadError{
			errors.Wrap(err, "profile: Failed to generate nacl key"),
		}
		return
	}
	senderPubKey64 := base64.StdEncoding.EncodeToString(senderPubKey[:])

	var nonce [24]byte
	nonceSl := make([]byte, 24)
	_, err = rand.Read(nonceSl)
	if err != nil {
		err = &errortypes.ReadError{
			errors.Wrap(err, "profile: Failed to generate nacl nonce"),
		}
		return
	}
	copy(nonce[:], nonceSl)

	encrypted := box.Seal([]byte{}, ovpnBoxData,
		&nonce, &serverPubKey, senderPrivKey)

	nonce64 := base64.StdEncoding.EncodeToString(nonceSl)
	ciphertext64 := base64.StdEncoding.EncodeToString(encrypted)

	ovpnReq := &WgKeyReq{
		Data:      ciphertext64,
		Nonce:     nonce64,
		PublicKey: senderPubKey64,
	}

	userPrivKeyBlock, _ := pem.Decode([]byte(p.PrivateKey))
	if userPrivKeyBlock == nil {
		err = &errortypes.ParseError{
			errors.New("profile: Failed to decode private key"),
		}
		return
	}

	userPrivKey, err := x509.ParsePKCS1PrivateKey(userPrivKeyBlock.Bytes)
	if err != nil {
		userPrivKeyInf, e := x509.ParsePKCS8PrivateKey(
			userPrivKeyBlock.Bytes)
		if e != nil {
			err = &errortypes.ParseError{
				errors.Wrap(e, "profile: Failed to parse private key"),
			}
			return
		}

		userPrivKey = userPrivKeyInf.(*rsa.PrivateKey)
	}

	reqHash := sha512.Sum512([]byte(strings.Join([]string{
		ovpnReq.Data,
		ovpnReq.Nonce,
		ovpnReq.PublicKey,
	}, "&")))

	rsaSig, err := rsa.SignPSS(
		rand.Reader,
		userPrivKey,
		crypto.SHA512,
		reqHash[:],
		&rsa.PSSOptions{
			SaltLength: 0,
			Hash:       crypto.SHA512,
		},
	)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to rsa sign data"),
		}
		return
	}

	ovpnReq.Signature = base64.StdEncoding.EncodeToString(rsaSig)

	ovpnReqData, err := json.Marshal(ovpnReq)
	if err != nil {
		return
	}

	reqPath := fmt.Sprintf(
		"/key/ovpn/%s/%s/%s",
		p.OrgId, p.UserId, p.ServerId,
	)

	if strings.Count(remote, ":") > 1 {
		remote = "[" + remote + "]"
	}

	u := &url.URL{
		Scheme: "https",
		Host:   remote,
		Path:   reqPath,
	}

	conx, cancel := context.WithCancel(context.Background())

	req, err := http.NewRequestWithContext(
		conx,
		"POST",
		u.String(),
		bytes.NewBuffer(ovpnReqData),
	)
	if err != nil {
		err = &errortypes.RequestError{
			errors.Wrap(err, "profile: Request put error"),
		}
		cancel()
		return
	}

	req.Header.Set("User-Agent", "pritunl-client")
	req.Header.Set("Content-Type", "application/json")

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	authNonce, err := utils.RandStr(32)
	if err != nil {
		cancel()
		return
	}

	authStr := strings.Join([]string{
		p.SyncToken,
		timestamp,
		authNonce,
		"POST",
		reqPath,
		ovpnReq.Data,
		ovpnReq.Nonce,
		ovpnReq.PublicKey,
		ovpnReq.Signature,
	}, "&")

	hashFunc := hmac.New(sha512.New, []byte(p.SyncSecret))
	hashFunc.Write([]byte(authStr))
	rawSignature := hashFunc.Sum(nil)
	sig := base64.StdEncoding.EncodeToString(rawSignature)

	req.Header.Set("Auth-Token", p.SyncToken)
	req.Header.Set("Auth-Timestamp", timestamp)
	req.Header.Set("Auth-Nonce", authNonce)
	req.Header.Set("Auth-Signature", sig)

	p.openReqCancel = cancel
	res, err := clientConnInsecure.Do(req)
	if err != nil {
		err = &errortypes.RequestError{
			errors.Wrap(err, "profile: Request put error"),
		}
		return
	}
	defer res.Body.Close()
	p.openReqCancel = nil

	if res.StatusCode != 200 {
		// TODO Show Server offline error for 429
		err = &errortypes.RequestError{
			errors.Wrapf(err, "profile: Bad status %d code from server",
				res.StatusCode),
		}
		return
	}

	ovpnResp := &KeyResp{}
	err = json.NewDecoder(res.Body).Decode(&ovpnResp)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to parse response body"),
		}
		return
	}

	respHashFunc := hmac.New(sha512.New, []byte(p.SyncSecret))
	respHashFunc.Write([]byte(ovpnResp.Data + "&" + ovpnResp.Nonce))
	respRawSignature := respHashFunc.Sum(nil)
	respSig := base64.StdEncoding.EncodeToString(respRawSignature)

	if subtle.ConstantTimeCompare(
		[]byte(respSig), []byte(ovpnResp.Signature)) != 1 {

		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Response signature invalid"),
		}
		return
	}

	respCiphertext, err := base64.StdEncoding.DecodeString(ovpnResp.Data)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to parse response data"),
		}
		return
	}

	var respNonce [24]byte
	respNonceSl, err := base64.StdEncoding.DecodeString(ovpnResp.Nonce)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to parse response nonce"),
		}
		return
	}
	copy(respNonce[:], respNonceSl)

	respPlaintext, ok := box.Open([]byte{}, respCiphertext,
		&respNonce, &serverPubKey, senderPrivKey)

	if !ok {
		err = &errortypes.ParseError{
			errors.New("profile: Failed to decrypt response"),
		}
		return
	}

	ovpnData = &OvpnData{}
	err = json.Unmarshal(respPlaintext, ovpnData)
	if err != nil {
		err = &errortypes.ParseError{
			errors.New("profile: Failed to parse response"),
		}
		return
	}

	return
}

func (p *Profile) reqWg(remote string) (wgData *WgData, err error) {
	if p.ServerBoxPublicKey == "" {
		err = &errortypes.ReadError{
			errors.Wrap(err, "profile: Server box public key not set"),
		}
		return
	}

	var serverPubKey [32]byte
	serverPubKeySlic, err := base64.StdEncoding.DecodeString(
		p.ServerBoxPublicKey)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to decode server box key"),
		}
		return
	}
	copy(serverPubKey[:], serverPubKeySlic)

	tokn := token.Get(p.Id, p.ServerPublicKey, p.ServerBoxPublicKey)
	p.token = tokn

	authToken := ""
	if tokn != nil {
		err = tokn.Update()
		if err != nil {
			return
		}

		authToken = tokn.Token
	} else {
		authToken, err = utils.RandStrComplex(16)
		if err != nil {
			return
		}
	}

	tokenNonce, err := utils.RandStr(16)
	if err != nil {
		return
	}

	pltfrm := ""
	switch runtime.GOOS {
	case "linux":
		pltfrm = "linux"
		break
	case "windows":
		pltfrm = "win"
		break
	case "darwin":
		pltfrm = "mac"
		break
	default:
		pltfrm = "unknown"
		break
	}

	wgBox := &WgKeyBox{
		DeviceId:       p.DeviceId,
		DeviceName:     p.DeviceName,
		Platform:       pltfrm,
		MacAddr:        p.MacAddr,
		MacAddrs:       p.MacAddrs,
		Token:          authToken,
		Nonce:          tokenNonce,
		Password:       p.Password,
		Timestamp:      time.Now().Unix(),
		PublicAddress:  "", // TODO SET ADDR
		PublicAddress6: "", // TODO SET ADDR
		WgPublicKey:    p.PublicKeyWg,
	}

	wgBoxData, err := json.Marshal(wgBox)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to marshal wg key box"),
		}
		return
	}

	senderPubKey, senderPrivKey, err := box.GenerateKey(rand.Reader)
	if err != nil {
		err = &errortypes.ReadError{
			errors.Wrap(err, "profile: Failed to generate nacl key"),
		}
		return
	}
	senderPubKey64 := base64.StdEncoding.EncodeToString(senderPubKey[:])

	var nonce [24]byte
	nonceSl := make([]byte, 24)
	_, err = rand.Read(nonceSl)
	if err != nil {
		err = &errortypes.ReadError{
			errors.Wrap(err, "profile: Failed to generate nacl nonce"),
		}
		return
	}
	copy(nonce[:], nonceSl)

	encrypted := box.Seal([]byte{}, wgBoxData,
		&nonce, &serverPubKey, senderPrivKey)

	nonce64 := base64.StdEncoding.EncodeToString(nonceSl)
	ciphertext64 := base64.StdEncoding.EncodeToString(encrypted)

	wgReq := &WgKeyReq{
		Data:      ciphertext64,
		Nonce:     nonce64,
		PublicKey: senderPubKey64,
	}

	userPrivKeyBlock, _ := pem.Decode([]byte(p.PrivateKey))
	if userPrivKeyBlock == nil {
		err = &errortypes.ParseError{
			errors.New("profile: Failed to decode private key"),
		}
		return
	}

	userPrivKey, err := x509.ParsePKCS1PrivateKey(userPrivKeyBlock.Bytes)
	if err != nil {
		userPrivKeyInf, e := x509.ParsePKCS8PrivateKey(
			userPrivKeyBlock.Bytes)
		if e != nil {
			err = &errortypes.ParseError{
				errors.Wrap(e, "profile: Failed to parse private key"),
			}
			return
		}

		userPrivKey = userPrivKeyInf.(*rsa.PrivateKey)
	}

	reqHash := sha512.Sum512([]byte(strings.Join([]string{
		wgReq.Data,
		wgReq.Nonce,
		wgReq.PublicKey,
	}, "&")))

	rsaSig, err := rsa.SignPSS(
		rand.Reader,
		userPrivKey,
		crypto.SHA512,
		reqHash[:],
		&rsa.PSSOptions{
			SaltLength: 0,
			Hash:       crypto.SHA512,
		},
	)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to rsa sign data"),
		}
		return
	}

	wgReq.Signature = base64.StdEncoding.EncodeToString(rsaSig)

	wgReqData, err := json.Marshal(wgReq)
	if err != nil {
		return
	}

	reqPath := fmt.Sprintf(
		"/key/wg/%s/%s/%s",
		p.OrgId, p.UserId, p.ServerId,
	)

	if strings.Count(remote, ":") > 1 {
		remote = "[" + remote + "]"
	}

	u := &url.URL{
		Scheme: "https",
		Host:   remote,
		Path:   reqPath,
	}

	conx, cancel := context.WithCancel(context.Background())

	req, err := http.NewRequestWithContext(
		conx,
		"POST",
		u.String(),
		bytes.NewBuffer(wgReqData),
	)
	if err != nil {
		err = &errortypes.RequestError{
			errors.Wrap(err, "profile: Request put error"),
		}
		cancel()
		return
	}

	req.Header.Set("User-Agent", "pritunl-client")
	req.Header.Set("Content-Type", "application/json")

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	authNonce, err := utils.RandStr(32)
	if err != nil {
		cancel()
		return
	}

	authStr := strings.Join([]string{
		p.SyncToken,
		timestamp,
		authNonce,
		"POST",
		reqPath,
		wgReq.Data,
		wgReq.Nonce,
		wgReq.PublicKey,
		wgReq.Signature,
	}, "&")

	hashFunc := hmac.New(sha512.New, []byte(p.SyncSecret))
	hashFunc.Write([]byte(authStr))
	rawSignature := hashFunc.Sum(nil)
	sig := base64.StdEncoding.EncodeToString(rawSignature)

	req.Header.Set("Auth-Token", p.SyncToken)
	req.Header.Set("Auth-Timestamp", timestamp)
	req.Header.Set("Auth-Nonce", authNonce)
	req.Header.Set("Auth-Signature", sig)

	p.openReqCancel = cancel
	res, err := clientConnInsecure.Do(req)
	if err != nil {
		err = &errortypes.RequestError{
			errors.Wrap(err, "profile: Request put error"),
		}
		return
	}
	defer res.Body.Close()
	p.openReqCancel = nil

	if res.StatusCode != 200 {
		err = &errortypes.RequestError{
			errors.Wrapf(err, "profile: Bad status %d code from server",
				res.StatusCode),
		}
		return
	}

	wgResp := &KeyResp{}
	err = json.NewDecoder(res.Body).Decode(&wgResp)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to parse response body"),
		}
		return
	}

	respHashFunc := hmac.New(sha512.New, []byte(p.SyncSecret))
	respHashFunc.Write([]byte(wgResp.Data + "&" + wgResp.Nonce))
	respRawSignature := respHashFunc.Sum(nil)
	respSig := base64.StdEncoding.EncodeToString(respRawSignature)

	if subtle.ConstantTimeCompare(
		[]byte(respSig), []byte(wgResp.Signature)) != 1 {

		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Response signature invalid"),
		}
		return
	}

	respCiphertext, err := base64.StdEncoding.DecodeString(wgResp.Data)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to parse response data"),
		}
		return
	}

	var respNonce [24]byte
	respNonceSl, err := base64.StdEncoding.DecodeString(wgResp.Nonce)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to parse response nonce"),
		}
		return
	}
	copy(respNonce[:], respNonceSl)

	respPlaintext, ok := box.Open([]byte{}, respCiphertext,
		&respNonce, &serverPubKey, senderPrivKey)

	if !ok {
		err = &errortypes.ParseError{
			errors.New("profile: Failed to decrypt response"),
		}
		return
	}

	wgData = &WgData{}
	err = json.Unmarshal(respPlaintext, wgData)
	if err != nil {
		err = &errortypes.ParseError{
			errors.New("profile: Failed to parse response"),
		}
		return
	}

	return
}

func (p *Profile) pingWg() (wgData *WgPingData, retry bool, err error) {
	remote := p.GatewayAddr

	if p.ServerBoxPublicKey == "" {
		err = &errortypes.ReadError{
			errors.Wrap(err, "profile: Server box public key not set"),
		}
		return
	}

	var serverPubKey [32]byte
	serverPubKeySlic, err := base64.StdEncoding.DecodeString(
		p.ServerBoxPublicKey)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to decode server box key"),
		}
		return
	}
	copy(serverPubKey[:], serverPubKeySlic)

	platform := ""
	switch runtime.GOOS {
	case "linux":
		platform = "linux"
		break
	case "windows":
		platform = "win"
		break
	case "darwin":
		platform = "mac"
		break
	default:
		platform = "unknown"
		break
	}

	wgBox := &WgKeyBox{
		DeviceId:    p.DeviceId,
		DeviceName:  p.DeviceName,
		Platform:    platform,
		MacAddr:     p.MacAddr,
		MacAddrs:    p.MacAddrs,
		Timestamp:   time.Now().Unix(),
		WgPublicKey: p.PublicKeyWg,
	}

	wgBoxData, err := json.Marshal(wgBox)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to marshal wg key box"),
		}
		return
	}

	senderPubKey, senderPrivKey, err := box.GenerateKey(rand.Reader)
	if err != nil {
		err = &errortypes.ReadError{
			errors.Wrap(err, "profile: Failed to generate nacl key"),
		}
		return
	}
	senderPubKey64 := base64.StdEncoding.EncodeToString(senderPubKey[:])

	var nonce [24]byte
	nonceSl := make([]byte, 24)
	_, err = rand.Read(nonceSl)
	if err != nil {
		err = &errortypes.ReadError{
			errors.Wrap(err, "profile: Failed to generate nacl nonce"),
		}
		return
	}
	copy(nonce[:], nonceSl)

	encrypted := box.Seal([]byte{}, wgBoxData,
		&nonce, &serverPubKey, senderPrivKey)

	nonce64 := base64.StdEncoding.EncodeToString(nonceSl)
	ciphertext64 := base64.StdEncoding.EncodeToString(encrypted)

	wgReq := &WgKeyReq{
		Data:      ciphertext64,
		Nonce:     nonce64,
		PublicKey: senderPubKey64,
	}

	userPrivKeyBlock, _ := pem.Decode([]byte(p.PrivateKey))
	if userPrivKeyBlock == nil {
		err = &errortypes.ParseError{
			errors.New("profile: Failed to decode private key"),
		}
		return
	}

	userPrivKey, err := x509.ParsePKCS1PrivateKey(userPrivKeyBlock.Bytes)
	if err != nil {
		userPrivKeyInf, e := x509.ParsePKCS8PrivateKey(
			userPrivKeyBlock.Bytes)
		if e != nil {
			err = &errortypes.ParseError{
				errors.Wrap(e, "profile: Failed to parse private key"),
			}
			return
		}

		userPrivKey = userPrivKeyInf.(*rsa.PrivateKey)
	}

	reqHash := sha512.Sum512([]byte(strings.Join([]string{
		wgReq.Data,
		wgReq.Nonce,
		wgReq.PublicKey,
	}, "&")))

	rsaSig, err := rsa.SignPSS(
		rand.Reader,
		userPrivKey,
		crypto.SHA512,
		reqHash[:],
		&rsa.PSSOptions{
			SaltLength: 0,
			Hash:       crypto.SHA512,
		},
	)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to rsa sign data"),
		}
		return
	}

	wgReq.Signature = base64.StdEncoding.EncodeToString(rsaSig)

	wgReqData, err := json.Marshal(wgReq)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to marshal data"),
		}
		return
	}

	reqPath := fmt.Sprintf(
		"/key/wg/%s/%s/%s",
		p.OrgId, p.UserId, p.ServerId,
	)

	if strings.Contains(remote, ":") {
		remote = "[" + remote + "]"
	}

	scheme := ""
	if p.WebNoSsl {
		scheme = "http"
		if p.WebPort != 0 && p.WebPort != 80 {
			remote += fmt.Sprintf(":%d", p.WebPort)
		}
	} else {
		scheme = "https"
		if p.WebPort != 0 && p.WebPort != 443 {
			remote += fmt.Sprintf(":%d", p.WebPort)
		}
	}

	u := &url.URL{
		Scheme: scheme,
		Host:   remote,
		Path:   reqPath,
	}

	req, err := http.NewRequest(
		"PUT",
		u.String(),
		bytes.NewBuffer(wgReqData),
	)
	if err != nil {
		err = &errortypes.RequestError{
			errors.Wrap(err, "profile: Request put error"),
		}
		return
	}

	req.Header.Set("User-Agent", "pritunl-client")
	req.Header.Set("Content-Type", "application/json")

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	authNonce, err := utils.RandStr(32)
	if err != nil {
		return
	}

	authStr := strings.Join([]string{
		p.SyncToken,
		timestamp,
		authNonce,
		"PUT",
		reqPath,
		wgReq.Data,
		wgReq.Nonce,
		wgReq.PublicKey,
		wgReq.Signature,
	}, "&")

	hashFunc := hmac.New(sha512.New, []byte(p.SyncSecret))
	hashFunc.Write([]byte(authStr))
	rawSignature := hashFunc.Sum(nil)
	sig := base64.StdEncoding.EncodeToString(rawSignature)

	req.Header.Set("Auth-Token", p.SyncToken)
	req.Header.Set("Auth-Timestamp", timestamp)
	req.Header.Set("Auth-Nonce", authNonce)
	req.Header.Set("Auth-Signature", sig)

	res, err := clientInsecure.Do(req)
	if err != nil {
		retry = true
		err = &errortypes.RequestError{
			errors.Wrap(err, "profile: Request put error"),
		}
		return
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		if res.StatusCode < 400 || res.StatusCode >= 500 {
			retry = true
		}

		err = &errortypes.RequestError{
			errors.Wrapf(err, "profile: Bad status %d code from server",
				res.StatusCode),
		}
		return
	}

	wgResp := &KeyResp{}
	err = json.NewDecoder(res.Body).Decode(&wgResp)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to parse response body"),
		}
		return
	}

	respHashFunc := hmac.New(sha512.New, []byte(p.SyncSecret))
	respHashFunc.Write([]byte(wgResp.Data + "&" + wgResp.Nonce))
	respRawSignature := respHashFunc.Sum(nil)
	respSig := base64.StdEncoding.EncodeToString(respRawSignature)

	if subtle.ConstantTimeCompare(
		[]byte(respSig), []byte(wgResp.Signature)) != 1 {

		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Response signature invalid"),
		}
		return
	}

	respCiphertext, err := base64.StdEncoding.DecodeString(wgResp.Data)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to parse response data"),
		}
		return
	}

	var respNonce [24]byte
	respNonceSl, err := base64.StdEncoding.DecodeString(wgResp.Nonce)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Failed to parse response nonce"),
		}
		return
	}
	copy(respNonce[:], respNonceSl)

	respPlaintext, ok := box.Open([]byte{}, respCiphertext,
		&respNonce, &serverPubKey, senderPrivKey)

	if !ok {
		err = &errortypes.ParseError{
			errors.New("profile: Failed to decrypt response"),
		}
		return
	}

	wgData = &WgPingData{}
	err = json.Unmarshal(respPlaintext, wgData)
	if err != nil {
		err = &errortypes.ParseError{
			errors.New("profile: Failed to parse response"),
		}
		return
	}

	return
}

func (p *Profile) confWgLinux(data *WgConf) (err error) {
	utils.ExecCombinedOutputLogged(
		[]string{
			"Cannot find device",
		},
		"ip", "link",
		"del", p.Iface,
	)

	_, err = utils.ExecCombinedOutputLogged(nil,
		"ip", "link",
		"add", "dev", p.Iface,
		"type", "wireguard",
	)
	if err != nil {
		return
	}

	_, err = utils.ExecCombinedOutputLogged(nil,
		"ip", "addr",
		"add", data.Address,
		"dev", p.Iface,
	)
	if err != nil {
		return
	}

	if data.Address6 != "" {
		_, err = utils.ExecCombinedOutputLogged(nil,
			"ip", "-6", "addr",
			"add", data.Address6,
			"dev", p.Iface,
		)
		if err != nil {
			return
		}
	}

	allowedIps := []string{}
	if data.Routes != nil {
		for _, route := range data.Routes {
			allowedIps = append(allowedIps, route.Network)
		}
	}
	if data.Routes6 != nil {
		for _, route := range data.Routes6 {
			allowedIps = append(allowedIps, route.Network)
		}
	}

	_, err = utils.ExecCombinedOutputLogged(nil,
		p.wgPath,
		"set", p.Iface,
		"private-key", p.wgConfPth,
		"peer", data.PublicKey,
		"persistent-keepalive", "10",
		"allowed-ips", strings.Join(allowedIps, ","),
		"endpoint", fmt.Sprintf("%s:%d", data.Hostname, data.Port),
	)
	if err != nil {
		return
	}

	_, err = utils.ExecCombinedOutputLogged(nil,
		"ip", "link",
		"set", p.Iface, "up",
	)
	if err != nil {
		return
	}

	if data.Routes != nil {
		p.Routes = data.Routes
		for _, route := range data.Routes {
			if route.NetGateway {

			} else {
				if route.Metric != 0 {
					_, err = utils.ExecCombinedOutputLogged(
						[]string{
							"File exists",
						},
						"ip", "route",
						"add", route.Network,
						"via", route.NextHop,
						"metric", strconv.Itoa(route.Metric),
						"dev", p.Iface,
					)
					if err != nil {
						return
					}
				} else {
					_, err = utils.ExecCombinedOutputLogged(
						[]string{
							"File exists",
						},
						"ip", "route",
						"add", route.Network,
						"via", route.NextHop,
						"dev", p.Iface,
					)
					if err != nil {
						return
					}
				}
			}
		}
	}

	if data.Routes6 != nil {
		p.Routes6 = data.Routes6
		for _, route := range data.Routes6 {
			if route.NetGateway {

			} else {
				if route.Metric != 0 {
					_, err = utils.ExecCombinedOutputLogged(
						[]string{
							"File exists",
						},
						"ip", "-6", "route",
						"add", route.Network,
						"via", route.NextHop,
						"metric", strconv.Itoa(route.Metric),
						"dev", p.Iface,
					)
					if err != nil {
						return
					}
				} else {
					_, err = utils.ExecCombinedOutputLogged(
						[]string{
							"File exists",
						},
						"ip", "-6", "route",
						"add", route.Network,
						"via", route.NextHop,
						"dev", p.Iface,
					)
					if err != nil {
						return
					}
				}
			}
		}
	}

	return
}

func (p *Profile) sendManagementCommand(cmd string) (err error) {
	conn, err := net.DialTimeout(
		"tcp",
		fmt.Sprintf("127.0.0.1:%d", p.managementPort),
		3*time.Second,
	)
	if err != nil {
		err = &errortypes.ReadError{
			errors.Wrap(err, "profile: Failed to open socket"),
		}
		return
	}
	defer conn.Close()

	go func() {
		for {
			buf := make([]byte, 10000)
			n, e := conn.Read(buf)
			if e != nil || n == 0 {
				break
			}
		}
	}()

	err = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if err != nil {
		err = &errortypes.ReadError{
			errors.Wrap(err, "profile: Failed set deadline"),
		}
		return
	}

	_, err = conn.Write([]byte(fmt.Sprintf("%s\n", p.managementPass)))
	if err != nil {
		err = &errortypes.ReadError{
			errors.Wrap(err, "profile: Failed to write socket password"),
		}
		return
	}

	time.Sleep(500 * time.Millisecond)

	_, err = conn.Write([]byte(fmt.Sprintf("%s\n", cmd)))
	if err != nil {
		err = &errortypes.ReadError{
			errors.Wrap(err, "profile: Failed to write socket command"),
		}
		return
	}

	return
}

func (p *Profile) confWgLinuxQuick() (err error) {
	p.wgQuickLock.Lock()
	defer p.wgQuickLock.Unlock()

	for i := 0; i < 3; i++ {
		_, _ = utils.ExecCombinedOutput(
			p.wgQuickPath, "down", p.Iface,
		)

		if i == 0 {
			time.Sleep(100 * time.Millisecond)
		} else {
			time.Sleep(500 * time.Millisecond)
		}

		_, err = utils.ExecCombinedOutputLogged(
			nil,
			p.wgQuickPath,
			"up", p.Iface,
		)
		if err == nil {
			break
		}
	}

	if err != nil {
		return
	}

	return
}

func (p *Profile) confWgMac() (err error) {
	p.wgQuickLock.Lock()
	defer p.wgQuickLock.Unlock()

	output := ""
	for i := 0; i < 3; i++ {
		_, _ = utils.ExecCombinedOutput(
			p.bashPath, p.wgQuickPath, "down", p.Iface,
		)

		if i == 0 {
			time.Sleep(100 * time.Millisecond)
		} else {
			time.Sleep(500 * time.Millisecond)
		}

		output, err = utils.ExecCombinedOutputLogged(
			nil,
			p.bashPath,
			p.wgQuickPath,
			"up", p.Iface,
		)
		if err == nil {
			break
		}
	}

	if err != nil {
		return
	}

	tunIface := ""
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "INFO") {
			match := wgIfaceMacReg.FindStringSubmatch(line)
			if match != nil && len(match) >= 2 {
				tunIface = match[1]
			}
		}
	}

	if tunIface == "" {
		for _, line := range strings.Split(output, "\n") {
			if strings.Contains(line, "Interface for") {
				lines := strings.Fields(line)
				tunIface = lines[len(lines)-1]
			}
		}
	}

	if tunIface == "" {
		err = &errortypes.ParseError{
			errors.New("profile: Failed to parse wg interface output"),
		}
		return
	}
	p.Tuniface = tunIface

	return
}

func (p *Profile) confWgWin() (err error) {
	for i := 0; i < 3; i++ {
		p.wgQuickLock.Lock()
		_, _ = utils.ExecCombinedOutput(
			"sc.exe", "stop", fmt.Sprintf("WireGuardTunnel$%s", p.Iface),
		)
		time.Sleep(100 * time.Millisecond)
		_, _ = utils.ExecCombinedOutput(
			"sc.exe", "delete", fmt.Sprintf("WireGuardTunnel$%s", p.Iface),
		)
		p.wgQuickLock.Unlock()

		if i == 0 {
			time.Sleep(100 * time.Millisecond)
		} else {
			time.Sleep(500 * time.Millisecond)
		}

		_, err = utils.ExecCombinedOutputLogged(
			nil,
			GetWgUtilPath(),
			"/installtunnelservice", p.wgConfPth,
		)
		if err == nil {
			break
		}
	}

	if err != nil {
		return
	}

	return
}

func (p *Profile) confWg(data *WgConf) (err error) {
	p.ClientAddr = data.Address
	p.ServerAddr = data.Hostname
	p.GatewayAddr = data.Gateway
	p.GatewayAddr6 = data.Gateway6
	p.WebPort = data.WebPort
	p.WebNoSsl = data.WebNoSsl
	p.wgServerPublicKey = data.PublicKey

	switch runtime.GOOS {
	case "darwin":
		err = p.confWgMac()
		break
	case "windows":
		err = p.confWgWin()
		break
	case "linux":
		err = p.confWgLinuxQuick()
		break
	default:
		panic("profile: Not implemented")
	}
	if err != nil {
		return
	}

	return
}

func (p *Profile) restart() {
	restartLock.Lock()
	if p.stop {
		restartLock.Unlock()
		return
	}

	prfl := p.Copy()
	restartLock.Unlock()

	err := p.Stop()
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Error("profile: Stop error")
		return
	}

	p.Wait()

	if prfl.Reconnect {
		err = prfl.Start(false, false)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"error": err,
			}).Error("profile: Restart error")
			return
		}
	}
}

func (p *Profile) updateWgHandshake() (err error) {
	iface := ""
	if runtime.GOOS == "darwin" {
		iface = p.Tuniface
	} else {
		iface = p.Iface
	}

	output, err := utils.ExecCombinedOutputLogged(
		[]string{
			"No such device",
			"access interface",
		},
		p.wgPath, "show", iface,
		"latest-handshakes",
	)
	if err != nil {
		return
	}

	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		if fields[0] == p.wgServerPublicKey {
			handshake, e := strconv.Atoi(fields[1])
			if e != nil {
				continue
			}

			p.wgHandshake = handshake
			return
		}
	}

	p.wgHandshake = 0
	return
}

func (p *Profile) watchWg() {
	defer func() {
		panc := recover()
		if panc != nil {
			logrus.WithFields(logrus.Fields{
				"stack": string(debug.Stack()),
				"panic": panc,
			}).Error("profile: Panic")
			panic(panc)
		}
	}()

	defer p.clearStatus(p.startTime)

	time.Sleep(1 * time.Second)

	for i := 0; i < 30; i++ {
		if p.stop {
			return
		}

		if i%10 == 0 {
			go p.pingWg()
		}

		err := p.updateWgHandshake()
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"error": err,
			}).Error("profile: Check handshake status failed")
			p.Stop()
			return
		}

		if p.stop {
			return
		}

		if p.wgHandshake != 0 {
			p.connected = true
			p.Status = "connected"
			p.Timestamp = time.Now().Unix() - 5
			p.update()
			break
		}

		time.Sleep(500 * time.Millisecond)
	}

	if p.wgHandshake == 0 {
		if p.stop {
			return
		}

		evt := event.Event{
			Type: "handshake_timeout",
			Data: p,
		}
		evt.Init()

		go p.restart()
		return
	}

	for {
		for i := 0; i < 10; i++ {
			if p.stop {
				return
			}
			time.Sleep(1 * time.Second)
		}

		var data *WgPingData
		var retry bool
		var err error
		for i := 0; i < 4; i++ {
			data, retry, err = p.pingWg()
			if !retry {
				break
			}

			time.Sleep(1 * time.Second)
		}
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"error": err,
			}).Error("profile: Keepalive failed")

			go p.restart()
			return
		}

		if p.stop {
			return
		}

		if data == nil || !data.Status {
			logrus.Error("profile: Keepalive bad status")

			go p.restart()
			return
		}
	}
}

func (p *Profile) startWg(timeout bool) (err error) {
	err = p.generateWgKey()
	if err != nil {
		p.clearStatus(p.startTime)
		return
	}

	p.update()

	remotesSet := set.NewSet()
	remotes := []string{}
	p.PrivateKey = ""

	ifaces, err := net.Interfaces()
	if err != nil {
		err = &errortypes.ReadError{
			errors.New("profile: Failed to load interfaces"),
		}

		p.clearStatus(p.startTime)
		return
	}

	macAddr := ""
	macAddrs := []string{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 ||
			iface.Flags&net.FlagLoopback != 0 ||
			iface.HardwareAddr == nil ||
			iface.HardwareAddr.String() == "" {

			continue
		}

		macAddr = iface.HardwareAddr.String()
		if p.MacAddr == "" {
			p.MacAddr = macAddr
		}
		macAddrs = append(macAddrs, macAddr)
	}
	p.MacAddrs = macAddrs

	rangeKey := false
	for _, line := range strings.Split(p.Data, "\n") {
		if !rangeKey {
			if strings.HasPrefix(line, "setenv UV_ID") {
				lineSpl := strings.Split(line, " ")
				if len(lineSpl) < 3 {
					continue
				}

				p.DeviceId = lineSpl[2]
			} else if strings.HasPrefix(line, "setenv UV_NAME") {
				lineSpl := strings.Split(line, " ")
				if len(lineSpl) < 3 {
					continue
				}

				p.DeviceName = lineSpl[2]
			} else if strings.HasPrefix(line, "remote ") {
				lineSpl := strings.Split(line, " ")
				if len(lineSpl) < 4 {
					continue
				}

				remote := lineSpl[1]
				if !remotesSet.Contains(remote) {
					remotesSet.Add(remote)
					remotes = append(remotes, remote)
				}
			} else if strings.HasPrefix(line, "<key>") {
				rangeKey = true
			}
		} else {
			if strings.HasPrefix(line, "</key>") {
				rangeKey = false
			} else {
				p.PrivateKey += line + "\n"
			}
		}
	}

	for _, syncAddr := range p.SyncHosts {
		syncUrl, e := url.Parse(syncAddr)
		if e != nil {
			err = &errortypes.ParseError{
				errors.Wrap(e, "profile: Sync address parse error"),
			}

			logrus.WithFields(logrus.Fields{
				"error": err,
			}).Error("profile: Failed to parse sync address")

			err = nil

			continue
		}

		remote := syncUrl.Host

		if !remotesSet.Contains(remote) {
			remotesSet.Add(remote)
			remotes = append(remotes, remote)
		}
	}

	var data *WgData
	for _, i := range mathrand.Perm(len(remotes)) {
		remote := remotes[i]

		data, err = p.reqWg(remote)
		if err == nil {
			break
		}

		if p.stop {
			p.clearStatus(p.startTime)
			return
		}
	}
	if err != nil {
		evt := event.Event{
			Type: "connection_error",
			Data: p,
		}
		evt.Init()

		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Error("profile: Request wg connection failed")
		err = nil

		time.Sleep(3 * time.Second)

		p.clearStatus(p.startTime)
		if p.connected && !p.stop {
			go p.restart()
		}
		return
	}

	if p.stop {
		p.clearStatus(p.startTime)
		return
	}

	if data == nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "profile: Request wg returned empty data"),
		}

		p.clearStatus(p.startTime)
		return
	}

	if !data.Allow {
		logrus.WithFields(logrus.Fields{
			"reason": data.Reason,
		}).Error("profile: Failed to authenticate wg")

		evt := event.Event{
			Type: "auth_error",
			Data: p,
		}
		evt.Init()

		if p.SystemProfile != nil {
			if p.SystemProfile.AuthErrorCount >= 2 {
				logrus.WithFields(logrus.Fields{
					"profile_id": p.SystemProfile.Id,
				}).Error("profile: Stopping system " +
					"profile due to authentication errors")

				p.SystemProfile.State = false
				sprofile.Deactivate(p.SystemProfile.Id)
				sprofile.SetAuthErrorCount(
					p.SystemProfile.Id,
					0,
				)
			} else {
				sprofile.SetAuthErrorCount(
					p.SystemProfile.Id,
					p.SystemProfile.AuthErrorCount+1,
				)
			}
		}

		time.Sleep(3 * time.Second)

		p.clearStatus(p.startTime)
		return
	}

	if data.Configuration == nil {
		err = &errortypes.ParseError{
			errors.Wrap(
				err,
				"profile: Request wg returned empty configuration",
			),
		}

		p.clearStatus(p.startTime)
		return
	}

	iface := network.InterfaceAcquire()
	if iface == "" {
		err = &errortypes.ReadError{
			errors.New("profile: Failed to acquire interface"),
		}

		p.clearStatus(p.startTime)
		return
	}
	p.Iface = iface

	wgConfPth, err := p.writeWgConf(data.Configuration)
	if err != nil {
		p.clearStatus(p.startTime)
		return
	}
	p.remPaths = append(p.remPaths, wgConfPth)
	p.wgConfPth = wgConfPth

	err = p.confWg(data.Configuration)
	if err != nil {
		evt := event.Event{
			Type: "configuration_error",
			Data: p,
		}
		evt.Init()

		logrus.WithFields(logrus.Fields{
			"error": err,
		}).Error("profile: Failed to configure wg")
		err = nil

		p.clearStatus(p.startTime)
		return
	}

	go p.watchWg()

	return
}

func (p *Profile) stopWgLinux() (err error) {
	//if p.Iface != "" {
	//	p.wgQuickLock.Lock()
	//	utils.ExecCombinedOutputLogged(
	//		[]string{
	//			"Cannot find device",
	//		},
	//		"ip", "link",
	//		"del", p.Iface,
	//	)
	//	p.wgQuickLock.Unlock()
	//}

	return
}

func (p *Profile) stopWgMac() (err error) {
	//if p.Iface != "" {
	//	p.wgQuickLock.Lock()
	//	utils.ExecCombinedOutputLogged(
	//		[]string{
	//			"is not a",
	//		},
	//		p.wgQuickPath,
	//		"down", p.Iface,
	//	)
	//	p.wgQuickLock.Unlock()
	//}

	return
}

func (p *Profile) stopWgWin() (err error) {
	//if p.Iface != "" {
	//	p.wgQuickLock.Lock()
	//	_, _ = utils.ExecCombinedOutput(
	//		"sc.exe", "stop", fmt.Sprintf("WireGuardTunnel$%s", p.Iface),
	//	)
	//	time.Sleep(100 * time.Millisecond)
	//	_, _ = utils.ExecCombinedOutput(
	//		"sc.exe", "delete", fmt.Sprintf("WireGuardTunnel$%s", p.Iface),
	//	)
	//	p.wgQuickLock.Unlock()
	//}

	return
}

func (p *Profile) stopWg() (err error) {
	switch runtime.GOOS {
	case "linux":
		err = p.stopWgLinux()
		break
	case "darwin":
		err = p.stopWgMac()
		break
	case "windows":
		err = p.stopWgWin()
		break
	default:
		panic("handlers: Not implemented")
	}
	if err != nil {
		return
	}

	return
}

func (p *Profile) stopOvpn() (err error) {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}

	if runtime.GOOS == "windows" {
		done := false

		go func() {
			defer func() {
				panc := recover()
				if panc != nil {
					logrus.WithFields(logrus.Fields{
						"stack": string(debug.Stack()),
						"panic": panc,
					}).Error("profile: Panic")
					panic(panc)
				}
			}()

			err = p.sendManagementCommand("signal SIGTERM")
			if err != nil {
				err = nil
				_ = p.cmd.Process.Kill()
				return
			}

			time.Sleep(5 * time.Second)
			if done {
				return
			}
			_ = p.cmd.Process.Kill()
		}()

		utils.ExecWaitTimeout(p.cmd.Process, 10*time.Second)
		done = true
	} else {
		p.cmd.Process.Signal(os.Interrupt)
		done := false

		go func() {
			defer func() {
				panc := recover()
				if panc != nil {
					logrus.WithFields(logrus.Fields{
						"stack": string(debug.Stack()),
						"panic": panc,
					}).Error("profile: Panic")
					panic(panc)
				}
			}()

			time.Sleep(5 * time.Second)
			if done {
				return
			}
			p.cmd.Process.Kill()
		}()

		utils.ExecWaitTimeout(p.cmd.Process, 10*time.Second)
		done = true
	}

	return
}

func (p *Profile) Stop() (err error) {
	p.stateLock.Lock()
	if p.stop {
		p.stateLock.Unlock()
		return
	}
	p.stop = true
	p.stateLock.Unlock()

	logrus.WithFields(logrus.Fields{
		"profile_id": p.Id,
	}).Info("profile: Disconnecting")

	p.Status = "disconnecting"
	p.update()

	cancel := p.openReqCancel
	if cancel != nil {
		cancel()
	}

	diff := utils.SinceAbs(p.startTime)
	if diff < 8*time.Second {
		delay := time.Duration(8-int64(diff.Seconds())) * time.Second
		time.Sleep(delay)
	}

	if p.Mode == Wg {
		err = p.stopWg()
	} else {
		err = p.stopOvpn()
	}

	return
}

func (p *Profile) Wait() {
	waiter := make(chan bool, 1)

	p.stateLock.Lock()
	if !p.state {
		return
	}
	p.waiters = append(p.waiters, waiter)
	p.stateLock.Unlock()

	<-waiter
	time.Sleep(50 * time.Millisecond)

	return
}
