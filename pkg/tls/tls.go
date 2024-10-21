/*
Copyright 2021 The cert-manager Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tls

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/cert-manager/cert-manager/pkg/util/pki"
	"github.com/go-logr/logr"
	"github.com/lestrrat-go/backoff/v2"
	"github.com/prometheus/client_golang/prometheus"
	"istio.io/istio/pkg/spiffe"
	pkiutil "istio.io/istio/security/pkg/pki/util"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/cert-manager/istio-csr/pkg/certmanager"
	"github.com/cert-manager/istio-csr/pkg/tls/rootca"
)

var (
	metricCertRequest = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "cert_manager_istio_csr",
			Name:      "tls_provider_certificate_requests",
			Help:      "Total number of certificate signing requests attempted for serving TLS. Success is 1 if there is no error, 0 otherwise.",
		}, []string{"success"},
	)
)

func init() {
	metrics.Registry.MustRegister(metricCertRequest)
}

// Interface is a TLS provider that serves consumers with the current root CA
// certificates, as well as exposing a tls.Config that can be used for serving.
type Interface interface {
	// TrustDomain returns the Trust Domain of the mesh.
	TrustDomain() string

	// RootCAs returns the root CA PEM bundle as well as an *x509.CertPool
	// containing the decoded CA certificates.
	// This func blocks until the CA certificates are available.
	RootCAs(ctx context.Context) *rootca.RootCAs

	// Config provides a tls.Config that is updated with updated serving
	// certificates and root CAs.
	// This func blocks until the tls.Config is available.
	Config(ctx context.Context) (*tls.Config, error)

	// SubscribeRootCAsEvent will return a channel that a message will be passed
	// when a root CA changes.
	SubscribeRootCAsEvent() <-chan event.GenericEvent
}

type Options struct {
	// TrustDomain is the trust domain to use for this mesh.
	TrustDomain string

	// RootCAsCertFile is an optional file location containing a PEM CA bundle.
	// If non-empty, this CA bundle will be used to populate the CA of the mesh.
	RootCAsCertFile string

	// ServingCertificateDuration is the duration requested for the gRPC service
	// serving certificate.
	ServingCertificateDuration time.Duration

	// ServingCertificateDNSNames is the DNS names that will be requested for the
	// gRPC service serving certificate. The service must be routable by clients
	// by at least one of these DNS names.
	ServingCertificateDNSNames []string

	// ServingCertificateKeySize is the number of bits to use for the serving
	// certificate RSAKeySize. The default is 2048.
	ServingCertificateKeySize int

	// ServingSignatureAlgorithm is the type of key of serving signature algorithm
	// used, RSA or ECDSA, The default is RSA.
	ServingSignatureAlgorithm string
}

// Provider is used to provide a tls config containing an automatically renewed
// private key and certificate. The provider will continue to renew the signed
// certificate and private in the background, while consumers can transparently
// use an exposed TLS config. Consumers *MUST* using this config as is, in
// order for the certificate and private key be renewed transparently.
type Provider struct {
	opts Options
	log  logr.Logger

	rootCAs rootca.RootCAs

	cm certmanager.Signer

	lock          sync.RWMutex
	tlsConfig     *tls.Config
	subscriptions []chan<- event.GenericEvent

	issuerChangeNotifier certmanager.IssuerChangeNotifier
}

// NewProvider will return a new provider where a TLS config is ready to be fetched.
func NewProvider(log logr.Logger, cm certmanager.Signer, opts Options, issuerChangeNotifier certmanager.IssuerChangeNotifier) (*Provider, error) {
	return &Provider{
		opts: opts,
		log:  log.WithName("tls-provider"),
		cm:   cm,

		issuerChangeNotifier: issuerChangeNotifier,
	}, nil
}

// Start will start the TLS provider. This will fetch a serving certificate and
// provide a TLS config based on it. Keep this certificate renewed. Blocking
// function.
func (p *Provider) Start(ctx context.Context) error {
	if len(p.opts.RootCAsCertFile) > 0 {
		rootCAsChan, err := rootca.Watch(ctx, p.log, p.opts.RootCAsCertFile)
		if err != nil {
			return err
		}

		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case rootCAs := <-rootCAsChan:
					p.lock.Lock()
					p.rootCAs = rootCAs
					// Broadcast update to subscribers
					for i := range p.subscriptions {
						go func(i int) { p.subscriptions[i] <- event.GenericEvent{} }(i)
					}
					p.lock.Unlock()
				}
			}
		}()
	}

	var notAfter time.Time

	backoffPolicy := backoff.Exponential(
		backoff.WithMinInterval(time.Second),
		backoff.WithMaxInterval(time.Second*30),
		backoff.WithJitterFactor(0.05),
		backoff.WithMaxRetries(250),
	)

	backoffController := backoffPolicy.Start(ctx)

	for backoff.Continue(backoffController) {
		// If using pure runtime configuration (i.e. no issuerRef provided on startup) we might need
		// to wait for an issuer to be available before we try to fetch the initial serving certificate.

		// We also need to wait to be able to actually successfully issue a certificate; if the
		// user is installing istio-csr and cert-manager at the same time and has already provisioned
		// a ConfigMap with runtime configuration before installing either, it's very possible that
		// the issuer the ConfigMap refers to doesn't yet exist, even though the issuer config exists

		if !p.issuerChangeNotifier.HasIssuerConfig() {
			p.issuerChangeNotifier.WaitForIssuerConfig(ctx)
		}

		// We have an issuerRef now (after waitForInitialIssuer) but we don't know that it's valid
		// since the issuer might not have been created yet; so we try, with a timeout, to fetch a
		// cert and if it fails we'll hit the backoff and try again

		issuanceTimeout := 10 * time.Second

		fetchCtx, cancelFunc := context.WithTimeout(ctx, issuanceTimeout)

		var err error
		notAfter, err = p.fetchCertificate(fetchCtx)
		if err != nil {
			cancelFunc()

			// Some of the functions which block in fetchCertificate don't wrap errors and we won't be able
			// to reliably confirm DeadlineExceeded was the actual error. But we can try and later the underlying
			// libraries might improve!
			if errors.Is(err, context.DeadlineExceeded) {
				p.log.Info(fmt.Sprintf("initial serving certificate didn't complete in %s; will retry", issuanceTimeout))
			} else {
				p.log.Info(fmt.Sprintf("failed to fetch initial serving certificate in %s: %s; will retry", issuanceTimeout, err))
			}

			continue
		}

		cancelFunc()
		break
	}

	p.log.Info("fetched initial serving certificate")

	for {
		// Create a new timer every loop. Renew 2/3 into certificate duration
		renewalTime := (2 * time.Until(notAfter)) / 3
		timer := time.NewTimer(renewalTime)

		if !notAfter.IsZero() {
			p.log.Info("waiting to renew certificate", "renewal-time", time.Now().Add(renewalTime))
		}

		select {
		case <-ctx.Done():
			p.log.Info("closing renewal", "context", ctx.Err())
			timer.Stop()

			p.lock.Lock()
			defer p.lock.Unlock()
			// Set nil so readiness returns false
			p.tlsConfig = nil

			return nil

		case <-timer.C:
			// Ensure we stop the timer after every tick to release resources
			timer.Stop()
		}

		// Renew certificate at every tick
		p.log.Info("renewing serving certificate")
		notAfter = p.mustFetchCertificate(ctx)
		p.log.Info("fetched new serving certificate", "expiry-time", notAfter)
	}
}

// mustFetchCertificate is a blocking func that will fetch a signed certificate
// for serving. Will not return until a signed certificate has been
// successfully fetched, or the context had been canceled.
// Returns the NotAfter timestamp of the signed certificate.
func (p *Provider) mustFetchCertificate(ctx context.Context) time.Time {
	// Time to attempt to fetch a new certificate if the last failed.
	ticker := time.NewTicker(time.Second * 20)
	defer ticker.Stop()

	for {
		// Fetch a new serving certificate, signed by cert-manager.
		notAfter, err := p.fetchCertificate(ctx)
		if err != nil {
			p.log.Error(err, "failed to fetch new serving certificate, retrying")

			// Cancel if the context has been canceled. Retry after tick.
			select {
			case <-ctx.Done():
				return time.Time{}
			case <-ticker.C:
				continue
			}
		}

		return notAfter
	}
}

// Config should be used by consumers of the provider to get a TLS config
// which will have the signed certificate and private key appropriately
// renewed.
// This function will block until a TLS config is ready or the context has been
// cancelled.
func (p *Provider) Config(ctx context.Context) (*tls.Config, error) {
	timer := time.NewTimer(time.Second / 4)
	defer timer.Stop()

	for {
		p.lock.RLock()
		conf := p.tlsConfig
		p.lock.RUnlock()

		if conf != nil {
			return &tls.Config{
				MinVersion:         tls.VersionTLS12,
				GetConfigForClient: p.getConfigForClient,
				ClientAuth:         tls.RequireAndVerifyClientCert,
			}, nil
		}

		select {
		case <-timer.C:
			timer.Reset(time.Second / 4)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// getConfigForClient will return a TLS config based upon the current signed
// certificate and private key the provider holds.
func (p *Provider) getConfigForClient(_ *tls.ClientHelloInfo) (*tls.Config, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.tlsConfig, nil
}

// RootCAs returns the configured CA certificate. This function blocks until
// the root CA has been populated.
func (p *Provider) RootCAs(ctx context.Context) *rootca.RootCAs {
	timer := time.NewTimer(time.Second)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			p.lock.RLock()
			rootCAs := p.rootCAs
			p.lock.RUnlock()

			if len(rootCAs.PEM) == 0 || rootCAs.CertPool == nil {
				timer.Reset(time.Second)
				continue
			}

			return &rootCAs
		}
	}
}

// fetchCertificate will attempt to fetch a new signed certificate with a new
// private key for serving. This will then be stored as the latest TLS config
// for this provider to be fetched by new client connections. If this process
// fails, returns error.
// Returns the NotAfter timestamp that the new signed certificate expires.
func (p *Provider) fetchCertificate(ctx context.Context) (time.Time, error) {
	// Increment certificate request metric by 1. Success label is 0 unless there
	// is no error where it is changed to 1.
	success := "0"
	defer func() { metricCertRequest.With(prometheus.Labels{"success": success}).Inc() }()

	opts := pkiutil.CertOptions{
		Host:     strings.Join(p.opts.ServingCertificateDNSNames, ","),
		IsServer: true,
		TTL:      p.opts.ServingCertificateDuration,
	}

	switch p.opts.ServingSignatureAlgorithm {
	case "RSA":
		opts.ECSigAlg = ""
		opts.RSAKeySize = p.opts.ServingCertificateKeySize
	case "ECDSA":
		opts.ECSigAlg = pkiutil.EcdsaSigAlg
		switch p.opts.ServingCertificateKeySize {
		case 256:
			opts.ECCCurve = pkiutil.P256Curve
		case 384:
			opts.ECCCurve = pkiutil.P384Curve
		default:
			return time.Time{}, fmt.Errorf("unsupported serving certificate key size (supported: 256, 384): %d", p.opts.ServingCertificateKeySize)
		}
	default:
		return time.Time{}, fmt.Errorf("unknown signature algorithm (supported: \"RSA\", \"ECDSA\"): %s", p.opts.ServingSignatureAlgorithm)
	}

	// Generate new CSR and private key for serving
	csr, pk, err := pkiutil.GenCSR(opts)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to generate serving private key and CSR: %s", err)
	}

	bundle, err := p.cm.Sign(ctx, "istio-csr-serving", csr, p.opts.ServingCertificateDuration, []cmapi.KeyUsage{cmapi.UsageServerAuth})
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to sign serving certificate: %w", err)
	}

	p.log.Info("serving certificate ready")

	// If we are not using a custom root CA, then overwrite the existing with
	// what was responded.
	if len(p.opts.RootCAsCertFile) == 0 {
		if err := p.loadCAsRoot(bundle.CA); err != nil {
			return time.Time{}, fmt.Errorf("failed to load CA from issuer response: %w", err)
		}
	}

	p.lock.Lock()
	defer p.lock.Unlock()

	// Parse the root CA
	if len(p.rootCAs.PEM) == 0 || p.rootCAs.CertPool == nil {
		return time.Time{}, errors.New("root CA certificate is not defined")
	}

	// Build the client certificate verifier based upon the root certificate
	peerCertVerifier := spiffe.NewPeerCertVerifier()
	if err := peerCertVerifier.AddMappingFromPEM(p.opts.TrustDomain, p.rootCAs.PEM); err != nil {
		return time.Time{}, fmt.Errorf("failed to add root CAs to SPIFFE peer certificate verifier: %w", err)
	}

	tlsCert, err := tls.X509KeyPair(bundle.Certificate, pk)
	if err != nil {
		return time.Time{}, err
	}

	leafCert, err := pki.DecodeX509CertificateBytes(bundle.Certificate)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse signed certificate: %w", err)
	}

	// Build the actual TLS config which will be used for serving and exposed by
	// this provider. This config will serve using the just signed certificate
	// and private key. Mutually authenticate incoming client requests based if a
	// certificate is present.
	p.tlsConfig = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{tlsCert},
		// Advertise ALPN, required in modern gRPC versions
		// Typically gRPC sets this for us, but since this tls.Config ultimately gets returned in GetConfigForClient it doesn't.
		NextProtos: []string{"h2"},
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  peerCertVerifier.GetGeneralCertPool(),
		VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
			err := peerCertVerifier.VerifyPeerCert(rawCerts, verifiedChains)
			if err != nil {
				p.log.Error(err, "could not verify certificate")
			}
			return err
		},
	}

	success = "1"

	return leafCert.NotAfter, nil
}

func (p *Provider) TrustDomain() string {
	return p.opts.TrustDomain
}

// All istio-csr pods need up-to-date serving certs to minimise the delay when a non-leader pod
// takes leadership.
func (p *Provider) NeedLeaderElection() bool {
	return false
}

// Check is used by the shared readiness manager to expose whether the tls
// provider is ready.
func (p *Provider) Check(_ *http.Request) error {
	p.lock.RLock()
	defer p.lock.RUnlock()

	if p.tlsConfig != nil {
		return nil
	}

	return errors.New("not ready")
}

// SubscribeRootCAsEvent will return a channel that a message will be passed
// when a root CA changes.
func (p *Provider) SubscribeRootCAsEvent() <-chan event.GenericEvent {
	p.lock.Lock()
	defer p.lock.Unlock()
	sub := make(chan event.GenericEvent)

	p.subscriptions = append(p.subscriptions, sub)
	return sub
}

// loadCAsRoot will load and update the current root CAs with the given root
// CAs PEM bundle. Is a no-op if the root CAs bundle has not changed.
// Sends an event to root CAs subscribers if the data has changed.
func (p *Provider) loadCAsRoot(rootCAsPEM []byte) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	// If the root CAs bundle has not been changed, return early
	if bytes.Equal(p.rootCAs.PEM, rootCAsPEM) {
		return nil
	}

	rootCAsCerts, err := pki.DecodeX509CertificateChainBytes(rootCAsPEM)
	if err != nil {
		return fmt.Errorf("failed to decode bundle CA returned from issuer: %w", err)
	}

	rootCAsPool := x509.NewCertPool()
	for _, rootCert := range rootCAsCerts {
		rootCAsPool.AddCert(rootCert)
	}

	p.rootCAs = rootca.RootCAs{PEM: rootCAsPEM, CertPool: rootCAsPool}
	for i := range p.subscriptions {
		go func(i int) { p.subscriptions[i] <- event.GenericEvent{} }(i)
	}

	return nil
}
