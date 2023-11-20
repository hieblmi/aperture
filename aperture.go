package aperture

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	_ "net/http/pprof" // Blank import to set up profiling HTTP handlers.
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	gateway "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	flags "github.com/jessevdk/go-flags"
	"github.com/lightninglabs/aperture/aperturedb"
	"github.com/lightninglabs/aperture/auth"
	"github.com/lightninglabs/aperture/challenger"
	"github.com/lightninglabs/aperture/lnc"
	"github.com/lightninglabs/aperture/mint"
	"github.com/lightninglabs/aperture/proxy"
	"github.com/lightninglabs/lightning-node-connect/hashmailrpc"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/cert"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/lightningnetwork/lnd/tor"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v2"
)

const (
	// topLevelKey is the top level key for an etcd cluster where we'll
	// store all LSAT proxy related data.
	topLevelKey = "lsat/proxy"

	// etcdKeyDelimeter is the delimeter we'll use for all etcd keys to
	// represent a path-like structure.
	etcdKeyDelimeter = "/"

	// selfSignedCertOrganization is the static string that we encode in the
	// organization field of a certificate if we create it ourselves.
	selfSignedCertOrganization = "aperture autogenerated cert"

	// selfSignedCertValidity is the certificate validity duration we are
	// using for aperture certificates. This is higher than lnd's default
	// 14 months and is set to a maximum just below what some operating
	// systems set as a sane maximum certificate duration. See
	// https://support.apple.com/en-us/HT210176 for more information.
	selfSignedCertValidity = time.Hour * 24 * 820

	// selfSignedCertExpiryMargin is how much time before the certificate's
	// expiry date we already refresh it with a new one. We set this to half
	// the certificate validity length to make the chances bigger for it to
	// be refreshed on a routine server restart.
	selfSignedCertExpiryMargin = selfSignedCertValidity / 2

	// hashMailGRPCPrefix is the prefix a gRPC request URI has when it is
	// meant for the hashmailrpc server to be handled.
	hashMailGRPCPrefix = "/hashmailrpc.HashMail/"

	// hashMailRESTPrefix is the prefix a REST request URI has when it is
	// meant for the hashmailrpc server to be handled.
	hashMailRESTPrefix = "/v1/lightning-node-connect/hashmail"

	// invoiceMacaroonName is the name of the invoice macaroon belonging
	// to the target lnd node.
	invoiceMacaroonName = "invoice.macaroon"

	// defaultMailboxAddress is the default address of the mailbox server
	// that will be used if none is specified.
	defaultMailboxAddress = "mailbox.terminal.lightning.today:443"
)

var (
	// http2TLSCipherSuites is the list of cipher suites we allow the server
	// to use. This list removes a CBC cipher from the list used in lnd's
	// cert package because the underlying HTTP/2 library treats it as a bad
	// cipher, according to https://tools.ietf.org/html/rfc7540#appendix-A
	// (also see golang.org/x/net/http2/ciphers.go).
	http2TLSCipherSuites = []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256,
	}

	// clientStreamingURIs is the list of REST URIs that are
	// client-streaming and shouldn't be closed after a single message.
	clientStreamingURIs = []*regexp.Regexp{
		regexp.MustCompile("^/v1/lightning-node-connect/hashmail/send$"),
	}
)

// Main is the true entrypoint of Aperture.
func Main() {
	// TODO: Prevent from running twice.
	err := run()

	// Unwrap our error and check whether help was requested from our flag
	// library. If the error is not wrapped, Unwrap returns nil. It is
	// still safe to check the type of this nil error.
	flagErr, isFlagErr := errors.Unwrap(err).(*flags.Error)
	isHelpErr := isFlagErr && flagErr.Type == flags.ErrHelp

	// If we got a nil error, or help was requested, just exit.
	if err == nil || isHelpErr {
		os.Exit(0)
	}

	// Print any other non-help related errors.
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// run sets up the proxy server and runs it. This function blocks until a
// shutdown signal is received.
func run() error {
	// Before starting everything, make sure we can intercept any interrupt
	// signals so we can block on waiting for them later.
	interceptor, err := signal.Intercept()
	if err != nil {
		return err
	}

	// Next, parse configuration file and set up logging.
	cfg, err := getConfig()
	if err != nil {
		return fmt.Errorf("unable to parse config file: %w", err)
	}
	err = setupLogging(cfg, interceptor)
	if err != nil {
		return fmt.Errorf("unable to set up logging: %v", err)
	}

	errChan := make(chan error)
	a := NewAperture(cfg)
	if err := a.Start(errChan); err != nil {
		return fmt.Errorf("unable to start aperture: %v", err)
	}

	select {
	case <-interceptor.ShutdownChannel():
		log.Infof("Received interrupt signal, shutting down aperture.")

	case err := <-errChan:
		log.Errorf("Error while running aperture: %v", err)
	}

	return a.Stop()
}

// Aperture is the main type of the aperture service. It holds all components
// that are required for the authenticating reverse proxy to do its job.
type Aperture struct {
	cfg *Config

	etcdClient    *clientv3.Client
	db            *sql.DB
	challenger    challenger.Challenger
	httpsServer   *http.Server
	torHTTPServer *http.Server
	proxy         *proxy.Proxy
	proxyCleanup  func()

	wg   sync.WaitGroup
	quit chan struct{}
}

// NewAperture creates a new instance of the Aperture service.
func NewAperture(cfg *Config) *Aperture {
	return &Aperture{
		cfg:  cfg,
		quit: make(chan struct{}),
	}
}

// Start sets up the proxy server and starts it.
func (a *Aperture) Start(errChan chan error) error {
	// Start the prometheus exporter.
	err := StartPrometheusExporter(a.cfg.Prometheus)
	if err != nil {
		return fmt.Errorf("unable to start the prometheus "+
			"exporter: %v", err)
	}

	// Enable http profiling and validate profile port number if requested.
	if a.cfg.Profile != 0 {
		if a.cfg.Profile < 1024 || a.cfg.Profile > 65535 {
			return fmt.Errorf("the profile port must be between " +
				"1024 and 65535")
		}

		go func() {
			http.Handle("/", http.RedirectHandler(
				"/debug/pprof", http.StatusSeeOther,
			))

			listenAddr := fmt.Sprintf("localhost:%d", a.cfg.Profile)

			log.Infof("Starting profile server at %s", listenAddr)
			fmt.Println(http.ListenAndServe(listenAddr, nil))
		}()
	}

	var (
		secretStore mint.SecretStore
		onionStore  tor.OnionStore
		lncStore    lnc.Store
	)

	// Connect to the chosen database backend.
	switch a.cfg.DatabaseBackend {
	case "etcd":
		// Initialize our etcd client.
		a.etcdClient, err = clientv3.New(clientv3.Config{
			Endpoints:   []string{a.cfg.Etcd.Host},
			DialTimeout: 5 * time.Second,
			Username:    a.cfg.Etcd.User,
			Password:    a.cfg.Etcd.Password,
		})
		if err != nil {
			return fmt.Errorf("unable to connect to etcd: %v", err)
		}

		secretStore = newSecretStore(a.etcdClient)
		onionStore = newOnionStore(a.etcdClient)

	case "postgres":
		db, err := aperturedb.NewPostgresStore(a.cfg.Postgres)
		if err != nil {
			return fmt.Errorf("unable to connect to postgres: %v",
				err)
		}
		a.db = db.DB

		dbSecretTxer := aperturedb.NewTransactionExecutor(db,
			func(tx *sql.Tx) aperturedb.SecretsDB {
				return db.WithTx(tx)
			},
		)
		secretStore = aperturedb.NewSecretsStore(dbSecretTxer)

		dbOnionTxer := aperturedb.NewTransactionExecutor(db,
			func(tx *sql.Tx) aperturedb.OnionDB {
				return db.WithTx(tx)
			},
		)
		onionStore = aperturedb.NewOnionStore(dbOnionTxer)

		dbLNCTxer := aperturedb.NewTransactionExecutor(db,
			func(tx *sql.Tx) aperturedb.LNCSessionsDB {
				return db.WithTx(tx)
			},
		)
		lncStore = aperturedb.NewLNCSessionsStore(dbLNCTxer)

	case "sqlite":
		db, err := aperturedb.NewSqliteStore(a.cfg.Sqlite)
		if err != nil {
			return fmt.Errorf("unable to connect to sqlite: %v",
				err)
		}
		a.db = db.DB

		dbSecretTxer := aperturedb.NewTransactionExecutor(db,
			func(tx *sql.Tx) aperturedb.SecretsDB {
				return db.WithTx(tx)
			},
		)
		secretStore = aperturedb.NewSecretsStore(dbSecretTxer)

		dbOnionTxer := aperturedb.NewTransactionExecutor(db,
			func(tx *sql.Tx) aperturedb.OnionDB {
				return db.WithTx(tx)
			},
		)
		onionStore = aperturedb.NewOnionStore(dbOnionTxer)

		dbLNCTxer := aperturedb.NewTransactionExecutor(db,
			func(tx *sql.Tx) aperturedb.LNCSessionsDB {
				return db.WithTx(tx)
			},
		)
		lncStore = aperturedb.NewLNCSessionsStore(dbLNCTxer)

	default:
		return fmt.Errorf("unknown database backend: %s",
			a.cfg.DatabaseBackend)
	}

	log.Infof("Using %v as database backend", a.cfg.DatabaseBackend)

	if !a.cfg.Authenticator.Disable {
		authCfg := a.cfg.Authenticator
		genInvoiceReq := func(price int64) (*lnrpc.Invoice, error) {
			return &lnrpc.Invoice{
				Memo:  "LSAT",
				Value: price,
			}, nil
		}

		switch {
		case authCfg.Passphrase != "":
			log.Infof("Using lnc's authenticator config")

			if a.cfg.DatabaseBackend == "etcd" {
				return fmt.Errorf("etcd is not supported as " +
					"a database backend for lnc " +
					"connections")
			}

			session, err := lnc.NewSession(
				authCfg.Passphrase, authCfg.MailboxAddress,
				authCfg.DevServer,
			)
			if err != nil {
				return fmt.Errorf("unable to create lnc "+
					"session: %w", err)
			}

			a.challenger, err = challenger.NewLNCChallenger(
				session, lncStore, genInvoiceReq, errChan,
			)
			if err != nil {
				return fmt.Errorf("unable to start lnc "+
					"challenger: %w", err)
			}

		case authCfg.LndHost != "":
			log.Infof("Using lnd's authenticator config")

			authCfg := a.cfg.Authenticator
			client, err := lndclient.NewBasicClient(
				authCfg.LndHost, authCfg.TLSPath,
				authCfg.MacDir, authCfg.Network,
				lndclient.MacFilename(
					invoiceMacaroonName,
				),
			)
			if err != nil {
				return err
			}

			a.challenger, err = challenger.NewLndChallenger(
				client, genInvoiceReq, context.Background,
				errChan,
			)
			if err != nil {
				return err
			}
		}
	}

	// Create the proxy and connect it to lnd.
	a.proxy, a.proxyCleanup, err = createProxy(
		a.cfg, a.challenger, secretStore,
	)
	if err != nil {
		return err
	}
	handler := http.HandlerFunc(a.proxy.ServeHTTP)
	a.httpsServer = &http.Server{
		Addr:         a.cfg.ListenAddr,
		Handler:      handler,
		IdleTimeout:  a.cfg.IdleTimeout,
		ReadTimeout:  a.cfg.ReadTimeout,
		WriteTimeout: a.cfg.WriteTimeout,
	}

	log.Infof("Creating server with idle_timeout=%v, read_timeout=%v "+
		"and write_timeout=%v", a.cfg.IdleTimeout, a.cfg.ReadTimeout,
		a.cfg.WriteTimeout)

	// Create TLS configuration by either creating new self-signed certs or
	// trying to obtain one through Let's Encrypt.
	var serveFn func() error
	if a.cfg.Insecure {
		// Normally, HTTP/2 only works with TLS. But there is a special
		// version called HTTP/2 Cleartext (h2c) that some clients
		// support and that gRPC uses when the grpc.WithInsecure()
		// option is used. The default HTTP handler doesn't support it
		// though so we need to add a special h2c handler here.
		serveFn = a.httpsServer.ListenAndServe
		a.httpsServer.Handler = h2c.NewHandler(handler, &http2.Server{})
	} else {
		a.httpsServer.TLSConfig, err = getTLSConfig(
			a.cfg.ServerName, a.cfg.BaseDir, a.cfg.AutoCert,
		)
		if err != nil {
			return err
		}
		serveFn = func() error {
			// The httpsServer.TLSConfig contains certificates at
			// this point so we don't need to pass in certificate
			// and key file names.
			return a.httpsServer.ListenAndServeTLS("", "")
		}
	}

	// Finally run the server.
	log.Infof("Starting the server, listening on %s.", a.cfg.ListenAddr)

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		select {
		case errChan <- serveFn():
		case <-a.quit:
		}
	}()

	// If we need to listen over Tor as well, we'll set up the onion
	// services now. We're not able to use TLS for onion services since they
	// can't be verified, so we'll spin up an additional HTTP/2 server
	// _without_ TLS that is not exposed to the outside world. This server
	// will only be reached through the onion services, which already
	// provide encryption, so running this additional HTTP server should be
	// relatively safe.
	if a.cfg.Tor.V3 {
		torController, err := initTorListener(a.cfg, onionStore)
		if err != nil {
			return err
		}
		defer func() {
			_ = torController.Stop()
		}()

		a.torHTTPServer = &http.Server{
			Addr:    fmt.Sprintf("localhost:%d", a.cfg.Tor.ListenPort),
			Handler: h2c.NewHandler(handler, &http2.Server{}),
		}
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()

			select {
			case errChan <- a.torHTTPServer.ListenAndServe():
			case <-a.quit:
			}
		}()
	}

	return nil
}

// UpdateServices instructs the proxy to re-initialize its internal
// configuration of backend services. This can be used to add or remove backends
// at run time or enable/disable authentication on the fly.
func (a *Aperture) UpdateServices(services []*proxy.Service) error {
	return a.proxy.UpdateServices(services)
}

// Stop gracefully shuts down the Aperture service.
func (a *Aperture) Stop() error {
	var returnErr error

	if a.challenger != nil {
		a.challenger.Stop()
	}

	// Stop everything that was started alongside the proxy, for example the
	// gRPC and REST servers.
	if a.proxyCleanup != nil {
		a.proxyCleanup()
	}

	if a.etcdClient != nil {
		if err := a.etcdClient.Close(); err != nil {
			log.Errorf("Error terminating etcd client: %v", err)
			returnErr = err
		}
	}

	if a.db != nil {
		if err := a.db.Close(); err != nil {
			log.Errorf("Error closing database: %v", err)
			returnErr = err
		}
	}

	// Shut down our client and server connections now. This should cause
	// the first goroutine to quit.
	cleanup(a.httpsServer, a.proxy)

	// If we started a tor server as well, shut it down now too to cause the
	// second goroutine to quit.
	if a.torHTTPServer != nil {
		if err := a.torHTTPServer.Close(); err != nil {
			log.Errorf("Error stopping tor server: %v", err)
			returnErr = err
		}
	}

	// Now we wait for the goroutines to exit before we return. The defers
	// will take care of the rest of our started resources.
	close(a.quit)
	a.wg.Wait()

	return returnErr
}

// fileExists reports whether the named file or directory exists.
// This function is taken from https://github.com/btcsuite/btcd
func fileExists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// getConfig loads and parses the configuration file then checks it for valid
// content.
func getConfig() (*Config, error) {
	// Pre-parse command line flags to determine whether we've been pointed
	// to a custom config file.
	cfg := NewConfig()
	if _, err := flags.Parse(cfg); err != nil {
		return nil, err
	}

	// If a custom config file is provided, we require that it exists.
	var mustExist bool

	// Start with our default path for config file.
	configFile := filepath.Join(apertureDataDir, defaultConfigFilename)

	// If a base directory is set, we'll look in there for a config file.
	// We don't require it to exist because we could just want to place
	// all of our files here, and specify all config inline.
	if cfg.BaseDir != "" {
		configFile = filepath.Join(cfg.BaseDir, defaultConfigFilename)
	}

	// If a specific config file is set, we'll look here for a config file,
	// even if a base directory for our files was set. In this case, the
	// config file must exist, since we're specifically being pointed it.
	if cfg.ConfigFile != "" {
		configFile = lnd.CleanAndExpandPath(cfg.ConfigFile)
		mustExist = true
	}

	// Read our config file, either from the custom path provided or our
	// default location.
	b, err := os.ReadFile(configFile)
	switch {
	// If the file was found, unmarshal it.
	case err == nil:
		err = yaml.Unmarshal(b, cfg)
		if err != nil {
			return nil, err
		}

	// If the error is unrelated to the existence of the file, we must
	// always return it.
	case !os.IsNotExist(err):
		return nil, err

	// If we require that the config file exists and we got an error
	// related to file existence, we must fail.
	case mustExist && os.IsNotExist(err):
		return nil, fmt.Errorf("config file: %v must exist: %w",
			configFile, err)
	}

	// Finally, parse the remaining command line options again to ensure
	// they take precedence.
	if _, err := flags.Parse(cfg); err != nil {
		return nil, err
	}

	// Clean and expand our base dir, cert and macaroon paths.
	cfg.BaseDir = lnd.CleanAndExpandPath(cfg.BaseDir)
	cfg.Authenticator.TLSPath = lnd.CleanAndExpandPath(
		cfg.Authenticator.TLSPath,
	)
	cfg.Authenticator.MacDir = lnd.CleanAndExpandPath(
		cfg.Authenticator.MacDir,
	)

	// Set default mailbox address if none is set.
	if cfg.Authenticator.MailboxAddress == "" {
		cfg.Authenticator.MailboxAddress = defaultMailboxAddress
	}

	// Then check the configuration that we got from the config file, all
	// required values need to be set at this point.
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// setupLogging parses the debug level and initializes the log file rotator.
func setupLogging(cfg *Config, interceptor signal.Interceptor) error {
	if cfg.DebugLevel == "" {
		cfg.DebugLevel = defaultLogLevel
	}

	// Now initialize the logger and set the log level.
	SetupLoggers(logWriter, interceptor)

	// Use our default data dir unless a base dir is set.
	logFile := filepath.Join(apertureDataDir, defaultLogFilename)
	if cfg.BaseDir != "" {
		logFile = filepath.Join(cfg.BaseDir, defaultLogFilename)
	}

	err := logWriter.InitLogRotator(
		logFile, defaultMaxLogFileSize, defaultMaxLogFiles,
	)
	if err != nil {
		return err
	}
	return build.ParseAndSetDebugLevels(cfg.DebugLevel, logWriter)
}

// getTLSConfig returns a TLS configuration for either a self-signed certificate
// or one obtained through Let's Encrypt.
func getTLSConfig(serverName, baseDir string, autoCert bool) (
	*tls.Config, error) {

	// Use our default data dir unless a base dir is set.
	apertureDir := apertureDataDir
	if baseDir != "" {
		apertureDir = baseDir
	}

	// If requested, use the autocert library that will create a new
	// certificate through Let's Encrypt as soon as the first client HTTP
	// request on the server using the TLS config comes in. Unfortunately
	// you cannot tell the library to create a certificate on startup for a
	// specific host.
	if autoCert {
		serverName := serverName
		if serverName == "" {
			return nil, fmt.Errorf("servername option is " +
				"required for secure operation")
		}

		certDir := filepath.Join(apertureDir, "autocert")
		log.Infof("Configuring autocert for server %v with cache dir "+
			"%v", serverName, certDir)

		manager := autocert.Manager{
			Cache:      autocert.DirCache(certDir),
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(serverName),
		}

		go func() {
			err := http.ListenAndServe(
				":http", manager.HTTPHandler(nil),
			)
			if err != nil {
				log.Errorf("autocert http: %v", err)
			}
		}()
		return &tls.Config{
			GetCertificate: manager.GetCertificate,
			CipherSuites:   http2TLSCipherSuites,
			MinVersion:     tls.VersionTLS10,
		}, nil
	}

	// If we're not using autocert, we want to create self-signed TLS certs
	// and save them at the specified location (if they don't already
	// exist).
	tlsKeyFile := filepath.Join(apertureDir, defaultTLSKeyFilename)
	tlsCertFile := filepath.Join(apertureDir, defaultTLSCertFilename)
	tlsExtraDomains := []string{serverName}
	if !fileExists(tlsCertFile) && !fileExists(tlsKeyFile) {
		log.Infof("Generating TLS certificates...")
		certBytes, keyBytes, err := cert.GenCertPair(
			selfSignedCertOrganization, nil, tlsExtraDomains, false,
			selfSignedCertValidity,
		)
		if err != nil {
			return nil, err
		}

		// Now that we have the certificate and key, we'll store them
		// to the file system.
		err = cert.WriteCertPair(
			tlsCertFile, tlsKeyFile, certBytes, keyBytes,
		)
		if err != nil {
			return nil, err
		}

		log.Infof("Done generating TLS certificates")
	}

	// Load the certs now so we can inspect it and return a complete TLS
	// config later.
	certData, parsedCert, err := cert.LoadCert(tlsCertFile, tlsKeyFile)
	if err != nil {
		return nil, err
	}

	// The margin is negative, so adding it to the expiry date should give
	// us a date in about the middle of it's validity period.
	expiryWithMargin := parsedCert.NotAfter.Add(
		-1 * selfSignedCertExpiryMargin,
	)

	// We only want to renew a certificate that we created ourselves. If
	// we are using a certificate that was passed to us (perhaps created by
	// an externally running Let's Encrypt process) we aren't going to try
	// to replace it.
	isSelfSigned := len(parsedCert.Subject.Organization) > 0 &&
		parsedCert.Subject.Organization[0] == selfSignedCertOrganization

	// If the certificate expired or it was outdated, delete it and the TLS
	// key and generate a new pair.
	if isSelfSigned && time.Now().After(expiryWithMargin) {
		log.Info("TLS certificate will expire soon, generating a " +
			"new one")

		err := os.Remove(tlsCertFile)
		if err != nil {
			return nil, err
		}

		err = os.Remove(tlsKeyFile)
		if err != nil {
			return nil, err
		}

		log.Infof("Renewing TLS certificates...")
		certBytes, keyBytes, err := cert.GenCertPair(
			selfSignedCertOrganization, nil, nil, false,
			selfSignedCertValidity,
		)
		if err != nil {
			return nil, err
		}

		err = cert.WriteCertPair(
			tlsCertFile, tlsKeyFile, certBytes, keyBytes,
		)
		if err != nil {
			return nil, err
		}

		log.Infof("Done renewing TLS certificates")

		// Reload the certificate data.
		certData, _, err = cert.LoadCert(tlsCertFile, tlsKeyFile)
		if err != nil {
			return nil, err
		}
	}
	return &tls.Config{
		Certificates: []tls.Certificate{certData},
		CipherSuites: http2TLSCipherSuites,
		MinVersion:   tls.VersionTLS10,
	}, nil
}

// initTorListener initiates a Tor controller instance with the Tor server
// specified in the config. Onion services will be created over which the proxy
// can be reached at.
func initTorListener(cfg *Config, store tor.OnionStore) (*tor.Controller,
	error) {

	// Establish a controller connection with the backing Tor server and
	// proceed to create the requested onion services.
	onionCfg := tor.AddOnionConfig{
		VirtualPort: int(cfg.Tor.VirtualPort),
		TargetPorts: []int{int(cfg.Tor.ListenPort)},
		Store:       store,
	}
	torController := tor.NewController(cfg.Tor.Control, "", "")
	if err := torController.Start(); err != nil {
		return nil, err
	}

	if cfg.Tor.V3 {
		onionCfg.Type = tor.V3
		addr, err := torController.AddOnion(onionCfg)
		if err != nil {
			return nil, err
		}

		log.Infof("Listening over Tor on %v", addr)
	}

	return torController, nil
}

// createProxy creates the proxy with all the services it needs.
func createProxy(cfg *Config, challenger challenger.Challenger,
	store mint.SecretStore) (*proxy.Proxy, func(), error) {

	minter := mint.New(&mint.Config{
		Challenger:     challenger,
		Secrets:        store,
		ServiceLimiter: newStaticServiceLimiter(cfg.Services),
		Now:            time.Now,
	})
	authenticator := auth.NewLsatAuthenticator(minter, challenger)

	// By default the static file server only returns 404 answers for
	// security reasons. Serving files from the staticRoot directory has to
	// be enabled intentionally.
	staticServer := http.NotFoundHandler()
	if cfg.ServeStatic {
		if len(strings.TrimSpace(cfg.StaticRoot)) == 0 {
			return nil, nil, fmt.Errorf("staticroot cannot be " +
				"empty, must contain path to directory that " +
				"contains index.html")
		}
		staticServer = http.FileServer(http.Dir(cfg.StaticRoot))
	}

	var (
		localServices []proxy.LocalService
		proxyCleanup  = func() {}
	)

	if cfg.HashMail.Enabled {
		hashMailServices, cleanup, err := createHashMailServer(cfg)
		if err != nil {
			return nil, nil, err
		}

		localServices = append(localServices, hashMailServices...)
		proxyCleanup = cleanup
	}

	// The static file server must be last since it will match all calls
	// that make it to it.
	localServices = append(localServices, proxy.NewLocalService(
		staticServer, func(r *http.Request) bool {
			return true
		},
	))

	prxy, err := proxy.New(authenticator, cfg.Services, localServices...)
	return prxy, proxyCleanup, err
}

// createHashMailServer creates the gRPC server for the hash mail message
// gateway and an additional REST and WebSocket capable proxy for that gRPC
// server.
func createHashMailServer(cfg *Config) ([]proxy.LocalService, func(), error) {
	var localServices []proxy.LocalService

	serverOpts := []grpc.ServerOption{
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime: time.Minute,
		}),
	}

	// Before we register both servers, we'll also ensure that the collector
	// will export latency metrics for the histogram.
	if cfg.Prometheus != nil && cfg.Prometheus.Enabled {
		grpc_prometheus.EnableHandlingTimeHistogram()
		serverOpts = append(
			serverOpts,
			grpc.ChainUnaryInterceptor(
				grpc_prometheus.UnaryServerInterceptor,
			),
			grpc.ChainStreamInterceptor(
				grpc_prometheus.StreamServerInterceptor,
			),
		)
	}

	// Create a gRPC server for the hashmail server.
	hashMailServer := newHashMailServer(hashMailServerConfig{
		msgRate:           cfg.HashMail.MessageRate,
		msgBurstAllowance: cfg.HashMail.MessageBurstAllowance,
		staleTimeout:      cfg.HashMail.StaleTimeout,
	})
	hashMailGRPC := grpc.NewServer(serverOpts...)
	hashmailrpc.RegisterHashMailServer(hashMailGRPC, hashMailServer)
	localServices = append(localServices, proxy.NewLocalService(
		hashMailGRPC, func(r *http.Request) bool {
			return strings.HasPrefix(r.URL.Path, hashMailGRPCPrefix)
		}),
	)

	// Export the gRPC information for the public gRPC server.
	if cfg.Prometheus != nil && cfg.Prometheus.Enabled {
		grpc_prometheus.Register(hashMailGRPC)
	}

	// And a REST proxy for it as well.
	// The default JSON marshaler of the REST proxy only sets OrigName to
	// true, which instructs it to use the same field names as specified in
	// the proto file and not switch to camel case. What we also want is
	// that the marshaler prints all values, even if they are falsey.
	customMarshalerOption := gateway.WithMarshalerOption(
		gateway.MIMEWildcard, &gateway.JSONPb{
			MarshalOptions: protojson.MarshalOptions{
				UseProtoNames:   true,
				EmitUnpopulated: true,
			},
		},
	)

	// We'll also create and start an accompanying proxy to serve clients
	// through REST.
	ctxc, cancel := context.WithCancel(context.Background())
	proxyCleanup := func() {
		hashMailServer.Stop()
		cancel()
	}

	// The REST proxy connects to our main listen address. If we're serving
	// TLS, we don't care about the certificate being valid, as we issue it
	// ourselves. If we are serving without TLS (for example when behind a
	// load balancer), we need to connect to ourselves without using TLS as
	// well.
	restProxyTLSOpt := grpc.WithTransportCredentials(credentials.NewTLS(
		&tls.Config{InsecureSkipVerify: true},
	))
	if cfg.Insecure {
		restProxyTLSOpt = grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		)
	}

	mux := gateway.NewServeMux(customMarshalerOption)
	err := hashmailrpc.RegisterHashMailHandlerFromEndpoint(
		ctxc, mux, cfg.ListenAddr, []grpc.DialOption{
			restProxyTLSOpt,
		},
	)
	if err != nil {
		proxyCleanup()

		return nil, nil, err
	}

	// Wrap the default grpc-gateway handler with the WebSocket handler.
	restHandler := lnrpc.NewWebSocketProxy(
		mux, log, 0, 0, clientStreamingURIs,
	)

	// Create our proxy chain now. A request will pass
	// through the following chain:
	// req ---> CORS handler --> WS proxy ---> REST proxy --> gRPC endpoint
	corsHandler := allowCORS(restHandler, []string{"*"})
	localServices = append(localServices, proxy.NewLocalService(
		corsHandler, func(r *http.Request) bool {
			return strings.HasPrefix(r.URL.Path, hashMailRESTPrefix)
		},
	))

	return localServices, proxyCleanup, nil
}

// cleanup closes the given server and shuts down the log rotator.
func cleanup(server io.Closer, proxy io.Closer) {
	if err := proxy.Close(); err != nil {
		log.Errorf("Error terminating proxy: %v", err)
	}
	err := server.Close()
	if err != nil {
		log.Errorf("Error closing server: %v", err)
	}
	log.Info("Shutdown complete")
	err = logWriter.Close()
	if err != nil {
		log.Errorf("Could not close log rotator: %v", err)
	}
}

// allowCORS wraps the given http.Handler with a function that adds the
// Access-Control-Allow-Origin header to the response.
func allowCORS(handler http.Handler, origins []string) http.Handler {
	allowHeaders := "Access-Control-Allow-Headers"
	allowMethods := "Access-Control-Allow-Methods"
	allowOrigin := "Access-Control-Allow-Origin"

	// If the user didn't supply any origins that means CORS is disabled
	// and we should return the original handler.
	if len(origins) == 0 {
		return handler
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// Skip everything if the browser doesn't send the Origin field.
		if origin == "" {
			handler.ServeHTTP(w, r)
			return
		}

		// Set the static header fields first.
		w.Header().Set(
			allowHeaders,
			"Content-Type, Accept, Grpc-Metadata-Macaroon",
		)
		w.Header().Set(allowMethods, "GET, POST, DELETE")

		// Either we allow all origins or the incoming request matches
		// a specific origin in our list of allowed origins.
		for _, allowedOrigin := range origins {
			if allowedOrigin == "*" || origin == allowedOrigin {
				// Only set allowed origin to requested origin.
				w.Header().Set(allowOrigin, origin)

				break
			}
		}

		// For a pre-flight request we only need to send the headers
		// back. No need to call the rest of the chain.
		if r.Method == "OPTIONS" {
			return
		}

		// Everything's prepared now, we can pass the request along the
		// chain of handlers.
		handler.ServeHTTP(w, r)
	})
}
