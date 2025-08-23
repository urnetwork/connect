package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	// "net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/net/proxy"
	"golang.org/x/term"

	"github.com/docopt/docopt-go"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

const DefaultApiUrl = "https://api.bringyour.com"
const DefaultConnectUrl = "wss://connect.bringyour.com"

// this value is set via the linker, e.g.
// -ldflags "-X main.Version=$WARP_VERSION-$WARP_VERSION_CODE"
var Version string

func init() {
	// debug.SetGCPercent(10)

	initGlog()

	// initPprof()
}

func initGlog() {
	flag.Set("logtostderr", "true")
	flag.Set("stderrthreshold", "INFO")
	flag.Set("v", "0")
	// unlike unix, the android/ios standard is for diagnostics to go to stdout
	os.Stderr = os.Stdout
}

func main() {
	usage := fmt.Sprintf(
		`Connect provider.

The default URLs are:
    api_url: %s
    connect_url: %s

Usage:
    provider auth ([<auth_code>] | --user_auth=<user_auth> [--password=<password>]) [-f]
    	[--api_url=<api_url>]
    	[--max-memory=<mem>]
    	[-v...]
    provider provide [--port=<port>]
        [--api_url=<api_url>]
        [--connect_url=<connect_url>]
        [--max-memory=<mem>]
        [-v...]
    provider auth-provide ([<auth_code>] | --user_auth=<user_auth> [--password=<password>]) [-f]
    	[--port=<port>]
        [--api_url=<api_url>]
        [--connect_url=<connect_url>]
        [--max-memory=<mem>]
        [-v...]
    provider proxy auth add [<key>] <proxy_user> <proxy_password> [-f]
    provider proxy auth remove [<key>]
    provider proxy add <key_address>... [-f]
    provider proxy remove <key_address>...
    
Options:
    -h --help                        Show this help and exit.
    --version                        Show version.
    -v...                            Enable verbose mode. -v implies verbose level 1,
    				                 -vv implies level 2... etc.
    -f                               Force overwrite the JWT token store file, if exists. By default,
    				                 if the JWT token store file already exists, it will not be overwritten.
    --api_url=<api_url>              Specify a custom API URL to use.
    --connect_url=<connect_url>      Specify a custom connect URL to use.
    --user_auth=<user_auth>	         Login with a username.
    --password=<password>            Login with a password. If --user_auth is used, you will be prompted for your
    				                 password anyways, if you don't specify it using this option.
    -p --port=<port>                 Status server port [default: 0].
    --max-memory=<mem>               Set the maximum amount of memory in bytes, or the suffixes b, kib, mib, gib may be used [This is a soft limit].
    <key>                            Authentication key
    <proxy_user>                     The SOCKS5 user per RFC 1928/1929
    <proxy_password>                 The SOCKS5 password per RFC 1928/1929
    <key_address>                    SOCKS5 server as host:port or key@host:port`,
		DefaultApiUrl,
		DefaultConnectUrl,
	)

	opts, err := docopt.ParseArgs(usage, os.Args[1:], RequireVersion())

	if err != nil {
		panic(err)
	}

	if proxy, _ := opts.Bool("proxy"); proxy {
		if auth, _ := opts.Bool("auth"); auth {
			if add, _ := opts.Bool("add"); add {
				proxyAuthAdd(opts)
			} else if remove, _ := opts.Bool("remove"); remove {
				proxyAuthRemove(opts)
			}
		} else if add, _ := opts.Bool("add"); add {
			proxyAdd(opts)
		} else if remove, _ := opts.Bool("remove"); remove {
			proxyRemove(opts)
		}
	} else if auth_, _ := opts.Bool("auth"); auth_ {
		auth(opts)
	} else if provide_, _ := opts.Bool("provide"); provide_ {
		provide(opts)
	} else if authProvide, _ := opts.Bool("auth-provide"); authProvide {
		auth(opts)
		provide(opts)
	}
}

func auth(opts docopt.Opts) {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	urNetworkDir := filepath.Join(home, ".urnetwork")
	jwtPath := filepath.Join(urNetworkDir, "jwt")

	if _, err := os.Stat(jwtPath); !errors.Is(err, os.ErrNotExist) {
		// jwt exists
		if force, _ := opts.Bool("-f"); !force {
			fmt.Printf("%s exists. Overwrite? [yN]\n", jwtPath)

			reader := bufio.NewReader(os.Stdin)
			confirm, _ := reader.ReadString('\n')
			if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
				return
			}

		}
	}

	apiUrl, err := opts.String("--api_url")
	if err != nil {
		apiUrl = DefaultApiUrl
	}

	maxMemoryHumanReadable, err := opts.String("--max-memory")
	var maxMemory connect.ByteCount
	if err == nil {
		maxMemory, err = connect.ParseByteCount(maxMemoryHumanReadable)
		if err != nil {
			panic(fmt.Errorf("Bad mem argument: %s", maxMemoryHumanReadable))
		}
	}
	if 0 < maxMemory {
		connect.ResizeMessagePools(maxMemory / 8)
		debug.SetMemoryLimit(maxMemory)
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	event := connect.NewEventWithContext(cancelCtx)
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	ctx := event.Ctx()

	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)

	api := connect.NewBringYourApi(ctx, clientStrategy, apiUrl)

	var byJwt string
	if userAuth, err := opts.String("--user_auth"); err == nil {
		// user_auth and password

		var password string
		if password, err = opts.String("--password"); err == nil && password == "" {
			fmt.Print("Enter password: ")
			passwordBytes, err := term.ReadPassword(int(syscall.Stdin))
			if err != nil {
				panic(err)
			}
			password = string(passwordBytes)
			fmt.Printf("\n")
		}

		// fmt.Printf("userAuth='%s'; password='%s'\n", userAuth, password)

		loginCallback, loginChannel := connect.NewBlockingApiCallback[*connect.AuthLoginWithPasswordResult](ctx)

		loginArgs := &connect.AuthLoginWithPasswordArgs{
			UserAuth: userAuth,
			Password: password,
		}

		api.AuthLoginWithPassword(loginArgs, loginCallback)

		var loginResult connect.ApiCallbackResult[*connect.AuthLoginWithPasswordResult]
		select {
		case <-ctx.Done():
			os.Exit(0)
		case loginResult = <-loginChannel:
		}

		if loginResult.Error != nil {
			panic(loginResult.Error)
		}
		if loginResult.Result.Error != nil {
			panic(fmt.Errorf("%s", loginResult.Result.Error.Message))
		}
		if loginResult.Result.VerificationRequired != nil {
			panic(fmt.Errorf("Verification required for %s. Use the app or web to complete account setup.", loginResult.Result.VerificationRequired.UserAuth))
		}

		byJwt = loginResult.Result.Network.ByJwt
	} else {
		// auth_code
		authCode, _ := opts.String("<auth_code>")
		if authCode == "" {
			fmt.Print("Enter auth code: ")
			authCodeBytes, err := term.ReadPassword(int(syscall.Stdin))
			if err != nil {
				panic(err)
			}
			authCode = strings.TrimSpace(string(authCodeBytes))
			fmt.Printf("\n")
		}

		authCodeLogin := &connect.AuthCodeLoginArgs{
			AuthCode: authCode,
		}

		authCodeLoginCallback, authCodeLoginChannel := connect.NewBlockingApiCallback[*connect.AuthCodeLoginResult](ctx)

		api.AuthCodeLogin(authCodeLogin, authCodeLoginCallback)

		var authCodeLoginResult connect.ApiCallbackResult[*connect.AuthCodeLoginResult]
		select {
		case <-ctx.Done():
			os.Exit(0)
		case authCodeLoginResult = <-authCodeLoginChannel:
		}

		if authCodeLoginResult.Error != nil {
			panic(authCodeLoginResult.Error)
		}
		if authCodeLoginResult.Result.Error != nil {
			panic(fmt.Errorf("%s", authCodeLoginResult.Result.Error.Message))
		}

		byJwt = authCodeLoginResult.Result.ByJwt
	}

	if byJwt != "" {
		if err := os.MkdirAll(urNetworkDir, 0700); err != nil {
			panic(err)
		}
		os.WriteFile(jwtPath, []byte(byJwt), 0700)
		fmt.Printf("Jwt written to %s\n", jwtPath)
	}
}

func provide(opts docopt.Opts) {
	port, _ := opts.Int("--port")

	apiUrl, err := opts.String("--api_url")
	if err != nil {
		apiUrl = DefaultApiUrl
	}

	connectUrl, err := opts.String("--connect_url")
	if err != nil {
		connectUrl = DefaultConnectUrl
	}

	maxMemoryHumanReadable, err := opts.String("--max-memory")
	var maxMemory connect.ByteCount
	if err == nil {
		maxMemory, err = connect.ParseByteCount(maxMemoryHumanReadable)
		if err != nil {
			panic(fmt.Errorf("Bad mem argument: %s", maxMemoryHumanReadable))
		}
	}
	if 0 < maxMemory {
		debug.SetMemoryLimit(maxMemory)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	event := connect.NewEventWithContext(ctx)
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	provideWithProxy := func(proxySettings *connect.ProxySettings) {
		proxyCtx, proxyCancel := context.WithCancel(event.Ctx())
		defer proxyCancel()

		clientStrategySettings := connect.DefaultClientStrategySettings()
		clientStrategySettings.ProxySettings = proxySettings
		clientStrategy := connect.NewClientStrategy(proxyCtx, clientStrategySettings)

		byClientJwt, clientId, err := provideAuth(proxyCtx, clientStrategy, apiUrl, opts)
		if err != nil {
			panic(err)
		}

		instanceId := connect.NewId()

		clientOob := connect.NewApiOutOfBandControl(proxyCtx, clientStrategy, byClientJwt, apiUrl)
		connectClient := connect.NewClientWithDefaults(proxyCtx, clientId, clientOob)
		defer connectClient.Close()

		// routeManager := connect.NewRouteManager(connectClient)
		// contractManager := connect.NewContractManagerWithDefaults(connectClient)
		// connectClient.Setup(routeManager, contractManager)
		// go connectClient.Run()

		fmt.Printf("client_id: %s\n", clientId)
		fmt.Printf("instance_id: %s\n", instanceId)

		auth := &connect.ClientAuth{
			ByJwt: byClientJwt,
			// ClientId: clientId,
			InstanceId: instanceId,
			AppVersion: RequireVersion(),
		}
		connect.NewPlatformTransportWithDefaults(proxyCtx, clientStrategy, connectClient.RouteManager(), connectUrl, auth)
		// go platformTransport.Run(connectClient.RouteManager())

		localUserNat := connect.NewLocalUserNatWithDefaults(proxyCtx, clientId.String())
		defer localUserNat.Close()
		remoteUserNatProvider := connect.NewRemoteUserNatProviderWithDefaults(connectClient, localUserNat)
		defer remoteUserNatProvider.Close()

		provideModes := map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Public:  true,
			protocol.ProvideMode_Network: true,
		}
		connectClient.ContractManager().SetProvideModes(provideModes)

		select {
		case <-proxyCtx.Done():
		}
	}

	var wg sync.WaitGroup

	if allProxySettings := readProxySettings(); 0 < len(allProxySettings) {
		fmt.Printf("Using %d proxy servers:\n", len(allProxySettings))
		for i, proxySettings := range allProxySettings {
			fmt.Printf("  proxy[%d] %s\n", i, proxySettings.Address)
		}
		for _, proxySettings := range allProxySettings {
			wg.Go(func() {
				provideWithProxy(proxySettings)
			})
		}
	} else {
		wg.Go(func() {
			provideWithProxy(nil)
		})
	}

	if 0 < port {
		fmt.Printf(
			"Provider %s started. Status on *:%d\n",
			RequireVersion(),
			port,
		)
		statusServer := &http.Server{
			Addr:    fmt.Sprintf(":%d", port),
			Handler: &Status{},
		}
		defer statusServer.Shutdown(ctx)

		go func() {
			defer cancel()
			err := statusServer.ListenAndServe()
			if err != nil {
				fmt.Printf("status error: %s\n", err)
			}
		}()
	} else {
		fmt.Printf(
			"Provider %s started\n",
			RequireVersion(),
		)
	}

	wg.Wait()

	select {
	case <-ctx.Done():
	}

	// exit
	os.Exit(0)
}

func provideAuth(ctx context.Context, clientStrategy *connect.ClientStrategy, apiUrl string, opts docopt.Opts) (byClientJwt string, clientId connect.Id, returnErr error) {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	jwtPath := filepath.Join(home, ".urnetwork", "jwt")

	if _, err := os.Stat(jwtPath); errors.Is(err, os.ErrNotExist) {
		// jwt does not exist
		returnErr = fmt.Errorf("Jwt does not exist at %s", jwtPath)
		return
	}

	byJwtBytes, err := os.ReadFile(jwtPath)
	if err != nil {
		returnErr = err
		return
	}
	byJwt := strings.TrimSpace(string(byJwtBytes))

	api := connect.NewBringYourApi(ctx, clientStrategy, apiUrl)

	api.SetByJwt(byJwt)

	authClientCallback, authClientChannel := connect.NewBlockingApiCallback[*connect.AuthNetworkClientResult](ctx)

	authClientArgs := &connect.AuthNetworkClientArgs{
		Description: fmt.Sprintf("provider %s %s", runtime.GOOS, RequireVersion()),
		DeviceSpec:  "",
	}

	api.AuthNetworkClient(authClientArgs, authClientCallback)

	var authClientResult connect.ApiCallbackResult[*connect.AuthNetworkClientResult]
	select {
	case <-ctx.Done():
		os.Exit(0)
	case authClientResult = <-authClientChannel:
	}

	if authClientResult.Error != nil {
		panic(authClientResult.Error)
	}
	if authClientResult.Result.Error != nil {
		panic(fmt.Errorf("%s", authClientResult.Result.Error.Message))
	}

	byClientJwt = authClientResult.Result.ByClientJwt

	// parse the clientId
	parser := gojwt.NewParser()
	token, _, err := parser.ParseUnverified(byClientJwt, gojwt.MapClaims{})
	if err != nil {
		panic(err)
	}

	claims := token.Claims.(gojwt.MapClaims)

	clientId, err = connect.ParseId(claims["client_id"].(string))
	if err != nil {
		panic(err)
	}

	return
}

type Status struct {
}

func (self *Status) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	type WarpStatusResult struct {
		Version       string `json:"version,omitempty"`
		ConfigVersion string `json:"config_version,omitempty"`
		Status        string `json:"status"`
		ClientAddress string `json:"client_address,omitempty"`
		Host          string `json:"host"`
	}

	result := &WarpStatusResult{
		Version: RequireVersion(),
		// ConfigVersion: RequireConfigVersion(),
		Status: "ok",
		Host:   RequireHost(),
	}

	responseJson, err := json.Marshal(result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(responseJson)
}

func Host() (string, error) {
	host := os.Getenv("WARP_HOST")
	if host != "" {
		return host, nil
	}
	host, err := os.Hostname()
	if err == nil {
		return host, nil
	}
	return "", errors.New("WARP_HOST not set")
}

func RequireHost() string {
	host, err := Host()
	if err != nil {
		panic(err)
	}
	return host
}

func RequireVersion() string {
	if version := os.Getenv("WARP_VERSION"); version != "" {
		return version
	}
	return Version
}

func proxyAuthAdd(opts docopt.Opts) {
	proxyConfig := readProxyConfig()

	key, _ := opts.String("key")
	user, _ := opts.String("proxy_user")
	password, _ := opts.String("proxy_password")

	if proxyConfig.Auths == nil {
		proxyConfig.Auths = map[string]*ProxyAuth{}
	}

	if _, ok := proxyConfig.Auths[key]; ok {
		if force, _ := opts.Bool("-f"); !force {
			fmt.Printf("auth key \"%s\" exists. Overwrite? [yN]\n", key)

			reader := bufio.NewReader(os.Stdin)
			confirm, _ := reader.ReadString('\n')
			if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
				return
			}
		}
	}

	proxyConfig.Auths[key] = &ProxyAuth{
		User:     user,
		Password: password,
	}

	writeProxyConfig(proxyConfig)
}

func proxyAuthRemove(opts docopt.Opts) {
	proxyConfig := readProxyConfig()

	key, _ := opts.String("key")

	if proxyConfig.Auths == nil {
		proxyConfig.Auths = map[string]*ProxyAuth{}
	}

	delete(proxyConfig.Auths, key)

	writeProxyConfig(proxyConfig)
}

func proxyAdd(opts docopt.Opts) {
	proxyConfig := readProxyConfig()

	allKeyAddressAny, _ := opts["<key_address>"]
	allKeyAddress := allKeyAddressAny.([]string)

	if proxyConfig.Servers == nil {
		proxyConfig.Servers = map[string]string{}
	}

	for _, keyAddress := range allKeyAddress {
		var key string
		var address string
		i := strings.Index(keyAddress, "@")
		if 0 <= i {
			key = keyAddress[:i]
			address = keyAddress[i+1:]
		} else {
			key = ""
			address = keyAddress
		}

		if _, ok := proxyConfig.Servers[address]; ok {
			if force, _ := opts.Bool("-f"); !force {
				fmt.Printf("server \"%s\" exists. Overwrite? [yN]\n", address)

				reader := bufio.NewReader(os.Stdin)
				confirm, _ := reader.ReadString('\n')
				if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
					return
				}
			}
		}

		proxyConfig.Servers[address] = key
	}

	writeProxyConfig(proxyConfig)
}

func proxyRemove(opts docopt.Opts) {
	proxyConfig := readProxyConfig()

	allKeyAddressAny, _ := opts["<key_address>"]
	allKeyAddress := allKeyAddressAny.([]string)

	if proxyConfig.Servers == nil {
		proxyConfig.Servers = map[string]string{}
	}

	for _, keyAddress := range allKeyAddress {
		var key string
		var address string
		i := strings.Index(keyAddress, "@")
		if 0 <= i {
			key = keyAddress[:i]
			address = keyAddress[i+1:]
		} else {
			key = ""
			address = keyAddress
		}

		if key == "" || proxyConfig.Servers[address] == key {
			delete(proxyConfig.Servers, address)
		}
	}

	writeProxyConfig(proxyConfig)
}

type ProxyConfig struct {
	Auths map[string]*ProxyAuth `json:"auths"`
	// TODO is there a use case for multiple keys to the same address?
	// address -> key
	Servers map[string]string `json:"servers"`
}

type ProxyAuth struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

func readProxySettings() []*connect.ProxySettings {
	proxyConfig := readProxyConfig()

	if proxyConfig.Servers == nil {
		return nil
	}

	var allProxySettings []*connect.ProxySettings
	for address, key := range proxyConfig.Servers {
		proxySettings := &connect.ProxySettings{
			Network: "tcp",
			Address: address,
		}
		if proxyConfig.Auths != nil {
			proxyAuth, ok := proxyConfig.Auths[key]
			if ok {
				proxySettings.Auth = &proxy.Auth{
					User:     proxyAuth.User,
					Password: proxyAuth.Password,
				}
			}
		}
		allProxySettings = append(allProxySettings, proxySettings)
	}

	return allProxySettings
}

func readProxyConfig() *ProxyConfig {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	urNetworkDir := filepath.Join(home, ".urnetwork")
	proxyPath := filepath.Join(urNetworkDir, "proxy")

	if _, err := os.Stat(proxyPath); errors.Is(err, os.ErrNotExist) {
		return &ProxyConfig{}
	}

	b, err := os.ReadFile(proxyPath)
	if err != nil {
		panic(err)
	}

	var proxyConfig ProxyConfig
	err = json.Unmarshal(b, &proxyConfig)
	if err != nil {
		panic(err)
	}
	return &proxyConfig
}

func writeProxyConfig(proxyConfig *ProxyConfig) {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	urNetworkDir := filepath.Join(home, ".urnetwork")
	proxyPath := filepath.Join(urNetworkDir, "proxy")

	b, err := json.Marshal(proxyConfig)
	if err != nil {
		panic(err)
	}

	err = os.WriteFile(proxyPath, b, 0700)
	if err != nil {
		panic(err)
	}
}
