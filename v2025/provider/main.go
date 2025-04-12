package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/docopt/docopt-go"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect/v2025"
	"github.com/urnetwork/connect/v2025/protocol"
)

const DefaultApiUrl = "https://api.bringyour.com"
const DefaultConnectUrl = "wss://connect.bringyour.com"

const LocalVersion = "0.0.0"

func main() {
	usage := fmt.Sprintf(
		`Connect provider.

The default urls are:
    api_url: %s
    connect_url: %s

Usage:
    provider auth ([<auth_code>] | --user_auth=<user_auth> [--password=<password>]) [-f]
    	[--api_url=<api_url>]
    provider provide [--port=<port>]
        [--api_url=<api_url>]
        [--connect_url=<connect_url>]
    provider auth-provide ([<auth_code>] | --user_auth=<user_auth> [--password=<password>]) [-f]
    	[--port=<port>]
        [--api_url=<api_url>]
        [--connect_url=<connect_url>]
    
Options:
    -h --help                        Show this screen.
    --version                        Show version.
    --api_url=<api_url>
    --connect_url=<connect_url>
    --user_auth=<user_auth>
    --password=<password>
    -p --port=<port>   Status server port [default: no status server].`,
		DefaultApiUrl,
		DefaultConnectUrl,
	)

	opts, err := docopt.ParseArgs(usage, os.Args[1:], RequireVersion())
	if err != nil {
		panic(err)
	}

	if auth_, _ := opts.Bool("auth"); auth_ {
		auth(opts)
	} else if provide_, _ := opts.Bool("provide"); provide_ {
		provide(opts)
	} else if authProvide_, _ := opts.Bool("auth-provide"); authProvide_ {
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

	cancelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	event := connect.NewEventWithContext(cancelCtx)
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	ctx := event.Ctx()

	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)

	byClientJwt, clientId, err := provideAuth(ctx, clientStrategy, apiUrl, opts)
	if err != nil {
		panic(err)
	}

	instanceId := connect.NewId()

	clientOob := connect.NewApiOutOfBandControl(ctx, clientStrategy, byClientJwt, apiUrl)
	connectClient := connect.NewClientWithDefaults(ctx, clientId, clientOob)

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
	connect.NewPlatformTransportWithDefaults(ctx, clientStrategy, connectClient.RouteManager(), connectUrl, auth)
	// go platformTransport.Run(connectClient.RouteManager())

	localUserNat := connect.NewLocalUserNatWithDefaults(ctx, clientId.String())
	remoteUserNatProvider := connect.NewRemoteUserNatProviderWithDefaults(connectClient, localUserNat)

	provideModes := map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Public:  true,
		protocol.ProvideMode_Network: true,
	}
	connectClient.ContractManager().SetProvideModes(provideModes)

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
	select {
	case <-ctx.Done():
	}

	remoteUserNatProvider.Close()
	localUserNat.Close()
	connectClient.Cancel()

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
		Description: fmt.Sprintf("provider %s", RequireVersion()),
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
		Version:       RequireVersion(),
		ConfigVersion: RequireConfigVersion(),
		Status:        "ok",
		Host:          RequireHost(),
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
	return fmt.Sprintf("%s-%s", LocalVersion, RequireHost())
}

func RequireConfigVersion() string {
	if version := os.Getenv("WARP_CONFIG_VERSION"); version != "" {
		return version
	}
	return fmt.Sprintf("%s-%s", LocalVersion, RequireHost())
}
