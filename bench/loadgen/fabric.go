package main

// Hyperledger Fabric backend (Fabric Gateway client, gateway on peer0.org1).

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hyperledger/fabric-gateway/pkg/client"
	"github.com/hyperledger/fabric-gateway/pkg/hash"
	"github.com/hyperledger/fabric-gateway/pkg/identity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

const (
	fabChannel = "rentalchannel"
	fabPeer    = "peer0.org1.example.com:7151"
)

var fabAttrOID = asn1.ObjectIdentifier{1, 2, 3, 4, 5, 6, 7, 8, 1}

// fabIdentity is a client identity with its signing key.
type fabIdentity struct {
	MSP     string `json:"msp"`
	CertPEM string `json:"cert"`
	KeyPEM  string `json:"key"`
	Account string `json:"account"` // cid.GetID value, resolved via the token chaincode
}

type fabState struct {
	Minter     fabIdentity   `json:"minter"`
	Arbitrator fabIdentity   `json:"arbitrator"`
	Landlords  []fabIdentity `json:"landlords"`
	Tenants    []fabIdentity `json:"tenants"`
	LeaseIDs   []string      `json:"leaseIds"`
}

// FabricBackend implements Backend.
type FabricBackend struct {
	crypto  string
	st      *fabState
	conns   []*grpc.ClientConn
	mu      sync.Mutex
	gws     map[string]*client.Gateway
	next    int
}

func newFabric(cryptoDir string) (*FabricBackend, error) {
	fb := &FabricBackend{crypto: cryptoDir, gws: map[string]*client.Gateway{}}
	caPEM, err := os.ReadFile(filepath.Join(cryptoDir, "peerOrganizations/org1.example.com/peers/peer0.org1.example.com/tls/ca.crt"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	creds := credentials.NewClientTLSFromCert(pool, "peer0.org1.example.com")
	for i := 0; i < 4; i++ { // small connection pool shared by all client identities
		c, err := grpc.NewClient("dns:///"+fabPeer, grpc.WithTransportCredentials(creds), grpc.WithNoProxy())
		if err != nil {
			return nil, err
		}
		fb.conns = append(fb.conns, c)
	}
	return fb, nil
}

func (fb *FabricBackend) Name() string   { return "fabric" }
func (fb *FabricBackend) NumLeases() int { return len(fb.st.LeaseIDs) }

func (fb *FabricBackend) Close() {
	for _, g := range fb.gws {
		g.Close()
	}
	for _, c := range fb.conns {
		c.Close()
	}
}

func (fb *FabricBackend) gateway(id fabIdentity) (*client.Gateway, error) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if g, ok := fb.gws[id.CertPEM]; ok {
		return g, nil
	}
	cert, err := identity.CertificateFromPEM([]byte(id.CertPEM))
	if err != nil {
		return nil, err
	}
	xid, err := identity.NewX509Identity(id.MSP, cert)
	if err != nil {
		return nil, err
	}
	key, err := identity.PrivateKeyFromPEM([]byte(id.KeyPEM))
	if err != nil {
		return nil, err
	}
	sign, err := identity.NewPrivateKeySign(key)
	if err != nil {
		return nil, err
	}
	conn := fb.conns[fb.next%len(fb.conns)]
	fb.next++
	g, err := client.Connect(xid, client.WithSign(sign), client.WithHash(hash.SHA256), client.WithClientConnection(conn),
		client.WithEvaluateTimeout(30*time.Second), client.WithEndorseTimeout(60*time.Second),
		client.WithSubmitTimeout(60*time.Second), client.WithCommitStatusTimeout(5*time.Minute))
	if err != nil {
		return nil, err
	}
	fb.gws[id.CertPEM] = g
	return g, nil
}

func (fb *FabricBackend) contract(id fabIdentity, cc string) (*client.Contract, error) {
	g, err := fb.gateway(id)
	if err != nil {
		return nil, err
	}
	return g.GetNetwork(fabChannel).GetContract(cc), nil
}

type fabPending struct{ commit *client.Commit }

func (p *fabPending) Wait(ctx context.Context) (bool, time.Time, string) {
	st, err := p.commit.StatusWithContext(ctx)
	now := time.Now()
	if err != nil {
		if ctx.Err() != nil {
			return false, time.Time{}, "timeout"
		}
		return false, time.Time{}, shorten(err.Error())
	}
	if !st.Successful {
		return false, now, "validation:" + st.Code.String()
	}
	return true, now, ""
}

// submit runs a transaction asynchronously and returns its pending commit.
func (fb *FabricBackend) submit(ctx context.Context, id fabIdentity, cc, fn string, args ...string) (Pending, error) {
	c, err := fb.contract(id, cc)
	if err != nil {
		return nil, err
	}
	_, commit, err := c.SubmitAsync(fn, client.WithArguments(args...))
	if err != nil {
		return nil, err
	}
	return &fabPending{commit: commit}, nil
}

// Submit implements Backend: a rent payment by the tenant of lease i.
func (fb *FabricBackend) Submit(ctx context.Context, lease int) (Pending, error) {
	return fb.submit(ctx, fb.st.Tenants[lease], "rental", "PayRent", fb.st.LeaseIDs[lease])
}

// exec submits a transaction and waits for its commit. Endorsement failures are retried a few
// times: with an all-organisations endorsement policy, a peer of another organisation may not yet
// have committed the block that the gateway peer just reported, and then simulates against stale
// state. (Used for setup and functional scenarios only; benchmark submissions are never retried.)
func (fb *FabricBackend) exec(ctx context.Context, id fabIdentity, cc, fn string, args ...string) error {
	var p Pending
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		if p, err = fb.submit(ctx, id, cc, fn, args...); err == nil {
			break
		}
		if !strings.Contains(err.Error(), "endorsements") && !strings.Contains(err.Error(), "Unavailable") {
			break
		}
		time.Sleep(time.Duration(300*(attempt+1)) * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", fn, endorseDetail(err))
	}
	ok, _, reason := p.Wait(ctx)
	if !ok {
		return fmt.Errorf("%s: %s", fn, reason)
	}
	return nil
}

func (fb *FabricBackend) evaluate(id fabIdentity, cc, fn string, args ...string) ([]byte, error) {
	c, err := fb.contract(id, cc)
	if err != nil {
		return nil, err
	}
	return c.EvaluateTransaction(fn, args...)
}

// ------------------------------------------------------------------ identities
type fabCA struct {
	cert *x509.Certificate
	key  any
	msp  string
}

func loadCA(cryptoDir string, org int) (*fabCA, error) {
	dir := filepath.Join(cryptoDir, fmt.Sprintf("peerOrganizations/org%d.example.com/ca", org))
	certPEM, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("ca.org%d.example.com-cert.pem", org)))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "priv_sk"))
	if err != nil {
		return nil, err
	}
	cb, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	kb, _ := pem.Decode(keyPEM)
	key, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	return &fabCA{cert: cert, key: key, msp: fmt.Sprintf("Org%dMSP", org)}, nil
}

// issue creates a client certificate (OU=client) signed by the org CA, embedding Fabric CA
// style attributes. notAfter lets tests create expired certificates.
func (ca *fabCA) issue(cn string, attrs map[string]string, notAfter time.Time) (fabIdentity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fabIdentity{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	pub, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	ski := sha256.Sum256(pub)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, OrganizationalUnit: []string{"client"}, Organization: ca.cert.Subject.Organization, Country: ca.cert.Subject.Country, Province: ca.cert.Subject.Province, Locality: ca.cert.Subject.Locality},
		// Fabric's MSP verifies the chain at the certificate's NotBefore, so it must not precede the CA's.
		NotBefore:             maxTime(time.Now().Add(-time.Minute), ca.cert.NotBefore.Add(time.Second)),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		SubjectKeyId:          ski[:],
	}
	if attrs != nil {
		b, _ := json.Marshal(map[string]any{"attrs": attrs})
		tmpl.ExtraExtensions = []pkix.Extension{{Id: fabAttrOID, Value: b}}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return fabIdentity{}, err
	}
	kder, _ := x509.MarshalPKCS8PrivateKey(key)
	return fabIdentity{
		MSP:     ca.msp,
		CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		KeyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder})),
	}, nil
}

func (fb *FabricBackend) resolveAccount(id *fabIdentity) error {
	b, err := fb.evaluate(*id, "token", "ClientAccountID")
	if err != nil {
		return err
	}
	id.Account = string(b)
	return nil
}

// fabTerms builds lease terms; period/numPeriods/windows are parameters so the same code
// serves the benchmark (1 s periods) and the end-to-end functional scenarios.
func fabTerms(l, t, a fabIdentity, property string, period, n, pw, cw, aw int64) string {
	b, _ := json.Marshal(map[string]any{
		"landlord": l.Account, "tenant": t.Account, "arbitrator": a.Account, "tokenChaincode": "token",
		"propertyId": property, "rentAmount": 500_000_000, "depositAmount": 1_000_000_000,
		"periodLength": period, "numPeriods": n, "proposalWindow": pw, "challengeWindow": cw, "arbitrationWindow": aw,
	})
	return string(b)
}

// Setup creates identities, initialises the token, and creates/activates `n` leases.
func (fb *FabricBackend) Setup(ctx context.Context, n int, stateFile string) error {
	cas := map[int]*fabCA{}
	for org := 1; org <= 3; org++ {
		ca, err := loadCA(fb.crypto, org)
		if err != nil {
			return err
		}
		cas[org] = ca
	}
	exp := time.Now().Add(365 * 24 * time.Hour)
	st := &fabState{}
	fb.st = st
	var err error
	if st.Minter, err = fb.minter(ctx, cas[1]); err != nil {
		return err
	}
	if st.Arbitrator, err = cas[3].issue("arbitrator", nil, exp); err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		prop := fmt.Sprintf("did:example:property:%d", i)
		l, err := cas[1].issue(fmt.Sprintf("landlord%d", i), map[string]string{"rental.role": "landlord", "rental.did": fmt.Sprintf("did:example:landlord-%d", i), "rental.property": prop}, exp)
		if err != nil {
			return err
		}
		t, err := cas[2].issue(fmt.Sprintf("tenant%d", i), map[string]string{"rental.role": "tenant", "rental.did": fmt.Sprintf("did:example:tenant-%d", i)}, exp)
		if err != nil {
			return err
		}
		st.Landlords, st.Tenants = append(st.Landlords, l), append(st.Tenants, t)
		st.LeaseIDs = append(st.LeaseIDs, fmt.Sprintf("bench-%d", i))
	}
	if err := fb.resolveAccount(&st.Arbitrator); err != nil {
		return err
	}
	err = parallel(n, 16, func(i int) error {
		if err := fb.resolveAccount(&st.Landlords[i]); err != nil {
			return err
		}
		if err := fb.resolveAccount(&st.Tenants[i]); err != nil {
			return err
		}
		steps := []func() error{
			func() error {
				return fb.exec(ctx, st.Minter, "token", "Mint", st.Tenants[i].Account, strconv.Itoa(1_000_000_000_000_000))
			},
			func() error {
				return fb.exec(ctx, st.Landlords[i], "rental", "CreateLease", st.LeaseIDs[i],
					fabTerms(st.Landlords[i], st.Tenants[i], st.Arbitrator, fmt.Sprintf("did:example:property:%d", i), 1, 65535, 14*86400, 14*86400, 30*86400))
			},
			func() error { return fb.exec(ctx, st.Landlords[i], "rental", "BindIdentity", st.LeaseIDs[i]) },
			func() error { return fb.exec(ctx, st.Tenants[i], "rental", "BindIdentity", st.LeaseIDs[i]) },
			func() error { return fb.exec(ctx, st.Tenants[i], "rental", "LockDeposit", st.LeaseIDs[i]) },
		}
		for _, s := range steps {
			if err := s(); err != nil {
				return fmt.Errorf("lease %d: %w", i, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return saveJSON(stateFile, st)
}

// MinterFile persists the token minter identity for the lifetime of a network.
var MinterFile = "/opt/bench/fabnet/minter.json"

// minter returns the identity that initialised the token chaincode, creating and initialising
// it on first use.
func (fb *FabricBackend) minter(ctx context.Context, ca *fabCA) (fabIdentity, error) {
	var m fabIdentity
	if err := loadJSON(MinterFile, &m); err == nil && m.Account != "" {
		return m, nil
	}
	m, err := ca.issue("minter", nil, time.Now().Add(365*24*time.Hour))
	if err != nil {
		return m, err
	}
	if err := fb.resolveAccount(&m); err != nil {
		return m, fmt.Errorf("resolve minter: %w", err)
	}
	if err := fb.exec(ctx, m, "token", "Initialize", "rental"); err != nil {
		return m, fmt.Errorf("token Initialize: %w", err)
	}
	return m, saveJSON(MinterFile, m)
}

func parseIdentity(id fabIdentity) (*x509.Certificate, any, error) {
	cb, _ := pem.Decode([]byte(id.CertPEM))
	c, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	kb, _ := pem.Decode([]byte(id.KeyPEM))
	k, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	return c, k, err
}

// Load restores state written by Setup.
func (fb *FabricBackend) Load(stateFile string) error {
	st := &fabState{}
	if err := loadJSON(stateFile, st); err != nil {
		return err
	}
	if len(st.LeaseIDs) == 0 {
		return errors.New("empty state")
	}
	fb.st = st
	return nil
}

func parallel(n, workers int, fn func(i int) error) error {
	jobs := make(chan int)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if err := fn(i); err != nil {
					errs <- err
				}
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	close(errs)
	for err := range errs {
		return err
	}
	return nil
}

func saveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func loadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// endorseDetail unwraps per-peer endorsement errors so that chaincode messages are visible.
func endorseDetail(err error) error {
	var ee *client.EndorseError
	if errors.As(err, &ee) {
		msg := err.Error()
		if st, ok := status.FromError(ee); ok {
			for _, d := range st.Details() {
				msg += fmt.Sprintf(" | %v", d)
			}
		}
		return errors.New(msg)
	}
	return err
}
