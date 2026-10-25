package main

// Hyperledger Besu (Ethereum) backend: raw transactions over JSON-RPC (HTTP) to node 1,
// commit observation through a newHeads subscription (WebSocket) on the same node.

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	besuHTTP    = "http://127.0.0.1:8545"
	besuWS      = "ws://127.0.0.1:8645"
	besuChainID = 1337
)

type contractArtifact struct {
	ABI      abi.ABI
	Bytecode []byte
}

func loadArtifact(outDir, file, name string) (*contractArtifact, error) {
	b, err := os.ReadFile(filepath.Join(outDir, file, name+".json"))
	if err != nil {
		return nil, err
	}
	var raw struct {
		ABI      json.RawMessage `json:"abi"`
		Bytecode struct {
			Object string `json:"object"`
		} `json:"bytecode"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	a, err := abi.JSON(strings.NewReader(string(raw.ABI)))
	if err != nil {
		return nil, err
	}
	code, err := hex.DecodeString(strings.TrimPrefix(raw.Bytecode.Object, "0x"))
	if err != nil {
		return nil, err
	}
	return &contractArtifact{ABI: a, Bytecode: code}, nil
}

// solTerms mirrors RentalAgreement.Terms for ABI encoding.
type solTerms struct {
	Landlord          common.Address
	Tenant            common.Address
	Arbitrator        common.Address
	Token             common.Address
	PropertyId        [32]byte
	RentAmount        *big.Int
	DepositAmount     *big.Int
	PeriodLength      uint32
	NumPeriods        uint16
	ProposalWindow    uint32
	ChallengeWindow   uint32
	ArbitrationWindow uint32
}

// solAttestation mirrors IdentityRegistry.Attestation.
type solAttestation struct {
	Subject    common.Address
	DidHash    [32]byte
	Role       uint8
	PropertyId [32]byte
	ExpiresAt  uint64
	Nonce      *big.Int
}

type besuState struct {
	Seed      string   `json:"seed"`
	Registry  string   `json:"registry"`
	Token     string   `json:"token"`
	Leases    []string `json:"leases"`
	Nonces    []uint64 `json:"tenantNonces"`
}

// account is a key with a locally tracked nonce.
type account struct {
	key   *ecdsa.PrivateKey
	addr  common.Address
	mu    sync.Mutex
	nonce uint64
}

func deriveKey(seed string, label string) *ecdsa.PrivateKey {
	k, _ := crypto.ToECDSA(crypto.Keccak256([]byte(seed + ":" + label)))
	return k
}

func newAccount(k *ecdsa.PrivateKey) *account {
	return &account{key: k, addr: crypto.PubkeyToAddress(k.PublicKey)}
}

// BesuBackend implements Backend.
type BesuBackend struct {
	ec      *ethclient.Client
	rc      *rpc.Client
	ws      *rpc.Client
	signer  types.Signer
	art     map[string]*contractArtifact
	st      *besuState
	deployer, issuer, arbitrator *account
	landlords, tenants           []*account
	watch   *blockWatcher
}

func newBesu(artifacts string) (*BesuBackend, error) {
	// Keep concurrent HTTP connections below Besu's default limit (80); requests queue in the
	// client instead of being refused by the node.
	hc := &http.Client{Transport: &http.Transport{Proxy: nil, MaxConnsPerHost: 64, MaxIdleConnsPerHost: 64}}
	rc, err := rpc.DialOptions(context.Background(), besuHTTP, rpc.WithHTTPClient(hc))
	if err != nil {
		return nil, err
	}
	ws, err := rpc.Dial(besuWS)
	if err != nil {
		return nil, err
	}
	bb := &BesuBackend{ec: ethclient.NewClient(rc), rc: rc, ws: ws,
		signer: types.LatestSignerForChainID(big.NewInt(besuChainID)), art: map[string]*contractArtifact{}}
	for file, name := range map[string]string{
		"RentalAgreement.sol": "RentalAgreement", "IdentityRegistry.sol": "IdentityRegistry", "MockStablecoin.sol": "PlainStablecoin"} {
		a, err := loadArtifact(artifacts, file, name)
		if err != nil {
			return nil, fmt.Errorf("artifact %s: %w", name, err)
		}
		bb.art[name] = a
	}
	bb.watch, err = startBlockWatcher(ws, rc)
	if err != nil {
		return nil, err
	}
	return bb, nil
}

func (bb *BesuBackend) Name() string   { return "besu" }
func (bb *BesuBackend) NumLeases() int { return len(bb.st.Leases) }
func (bb *BesuBackend) Close()         { bb.watch.stop(); bb.ws.Close(); bb.rc.Close() }

func (bb *BesuBackend) accounts(seed string, n int) {
	bb.deployer = newAccount(deriveKey(seed, "deployer"))
	bb.issuer = newAccount(deriveKey(seed, "issuer"))
	bb.arbitrator = newAccount(deriveKey(seed, "arbitrator"))
	bb.landlords, bb.tenants = nil, nil
	for i := 0; i < n; i++ {
		bb.landlords = append(bb.landlords, newAccount(deriveKey(seed, fmt.Sprintf("landlord-%d", i))))
		bb.tenants = append(bb.tenants, newAccount(deriveKey(seed, fmt.Sprintf("tenant-%d", i))))
	}
}

// send signs and submits a transaction from `from`; to==nil deploys.
func (bb *BesuBackend) send(ctx context.Context, from *account, to *common.Address, data []byte, gas uint64) (Pending, common.Hash, error) {
	from.mu.Lock()
	tx := types.NewTx(&types.LegacyTx{Nonce: from.nonce, GasPrice: big.NewInt(0), Gas: gas, To: to, Data: data})
	signed, err := types.SignTx(tx, bb.signer, from.key)
	if err != nil {
		from.mu.Unlock()
		return nil, common.Hash{}, err
	}
	p := bb.watch.expect(signed.Hash())
	if err := bb.ec.SendTransaction(ctx, signed); err != nil {
		// The node may or may not have accepted the transaction; resynchronise the nonce.
		if n, nerr := bb.ec.PendingNonceAt(ctx, from.addr); nerr == nil {
			from.nonce = n
		}
		from.mu.Unlock()
		bb.watch.forget(signed.Hash())
		return nil, common.Hash{}, err
	}
	from.nonce++
	from.mu.Unlock()
	return p, signed.Hash(), nil
}

func (bb *BesuBackend) call(art, method string, args ...any) []byte {
	d, err := bb.art[art].ABI.Pack(method, args...)
	if err != nil {
		panic(fmt.Sprintf("pack %s: %v", method, err))
	}
	return d
}

// exec sends and waits for a successful receipt.
func (bb *BesuBackend) exec(ctx context.Context, from *account, to *common.Address, data []byte, gas uint64, what string) (*types.Receipt, error) {
	p, h, err := bb.send(ctx, from, to, data, gas)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if ok, _, reason := p.Wait(ctx); !ok {
		return nil, fmt.Errorf("%s: %s", what, reason)
	}
	r, err := bb.ec.TransactionReceipt(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("%s receipt: %w", what, err)
	}
	if r.Status != types.ReceiptStatusSuccessful {
		return r, fmt.Errorf("%s reverted", what)
	}
	return r, nil
}

func (bb *BesuBackend) deploy(ctx context.Context, from *account, art string, args ...any) (common.Address, error) {
	ctor, err := bb.art[art].ABI.Pack("", args...)
	if err != nil {
		return common.Address{}, err
	}
	r, err := bb.exec(ctx, from, nil, append(append([]byte{}, bb.art[art].Bytecode...), ctor...), 5_000_000, "deploy "+art)
	if err != nil {
		return common.Address{}, err
	}
	return r.ContractAddress, nil
}

// attestation digest (EIP-712), computed locally and cross-checked once against the contract.
func attestationDigest(registry common.Address, a solAttestation) common.Hash {
	typeHash := crypto.Keccak256([]byte("Attestation(address subject,bytes32 didHash,uint8 role,bytes32 propertyId,uint64 expiresAt,uint256 nonce)"))
	domainType := crypto.Keccak256([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
	pad := func(b []byte) []byte { return common.LeftPadBytes(b, 32) }
	domain := crypto.Keccak256(domainType, crypto.Keccak256([]byte("RentalIdentityRegistry")), crypto.Keccak256([]byte("1")),
		pad(big.NewInt(besuChainID).Bytes()), pad(registry.Bytes()))
	structHash := crypto.Keccak256(typeHash, pad(a.Subject.Bytes()), a.DidHash[:], pad([]byte{a.Role}), a.PropertyId[:],
		pad(new(big.Int).SetUint64(a.ExpiresAt).Bytes()), pad(a.Nonce.Bytes()))
	return common.BytesToHash(crypto.Keccak256([]byte{0x19, 0x01}, domain, structHash))
}

func (bb *BesuBackend) attest(registry common.Address, subject common.Address, did string, role uint8, property [32]byte) (solAttestation, []byte) {
	a := solAttestation{Subject: subject, DidHash: crypto.Keccak256Hash([]byte(did)), Role: role, PropertyId: property,
		ExpiresAt: uint64(time.Now().Add(365 * 24 * time.Hour).Unix()), Nonce: new(big.Int).SetBytes(crypto.Keccak256([]byte(did)))}
	sig, _ := crypto.Sign(attestationDigest(registry, a).Bytes(), bb.issuer.key)
	sig[64] += 27
	return a, sig
}

func benchTerms(l, t, a, tok common.Address, prop [32]byte, period uint32, n uint16, pw, cw, aw uint32) solTerms {
	return solTerms{Landlord: l, Tenant: t, Arbitrator: a, Token: tok, PropertyId: prop,
		RentAmount: big.NewInt(500_000_000), DepositAmount: big.NewInt(1_000_000_000),
		PeriodLength: period, NumPeriods: n, ProposalWindow: pw, ChallengeWindow: cw, ArbitrationWindow: aw}
}

// Setup deploys the registry, token and `n` leases and activates every lease.
func (bb *BesuBackend) Setup(ctx context.Context, n int, stateFile string) error {
	seed := fmt.Sprintf("bench-%d", time.Now().UnixNano())
	bb.accounts(seed, n)
	for _, a := range append([]*account{bb.deployer, bb.issuer, bb.arbitrator}, append(bb.landlords, bb.tenants...)...) {
		nonce, err := bb.ec.PendingNonceAt(ctx, a.addr)
		if err != nil {
			return err
		}
		a.nonce = nonce
	}
	registry, err := bb.deploy(ctx, bb.deployer, "IdentityRegistry", bb.deployer.addr)
	if err != nil {
		return err
	}
	if _, err := bb.exec(ctx, bb.deployer, &registry, bb.call("IdentityRegistry", "addIssuer", bb.issuer.addr), 200_000, "addIssuer"); err != nil {
		return err
	}
	token, err := bb.deploy(ctx, bb.deployer, "PlainStablecoin")
	if err != nil {
		return err
	}
	// cross-check the locally computed EIP-712 digest against the contract once
	a0, _ := bb.attest(registry, bb.tenants[0].addr, "did:example:tenant-0", 2, [32]byte{})
	out, err := bb.ec.CallContract(ctx, ethCall(registry, bb.call("IdentityRegistry", "digest", a0)), nil)
	if err != nil {
		return err
	}
	if common.BytesToHash(out) != attestationDigest(registry, a0) {
		return errors.New("EIP-712 digest mismatch between client and contract")
	}

	st := &besuState{Seed: seed, Registry: registry.Hex(), Token: token.Hex(), Leases: make([]string, n)}
	// deployments and mints come from one deployer account: sequential nonces, pipelined
	leases := make([]common.Address, n)
	for start := 0; start < n; start += 50 {
		end := min(start+50, n)
		var pend []Pending
		var hashes []common.Hash
		for i := start; i < end; i++ {
			prop := crypto.Keccak256Hash([]byte(fmt.Sprintf("did:example:property:%d", i)))
			ctor, _ := bb.art["RentalAgreement"].ABI.Pack("", benchTerms(bb.landlords[i].addr, bb.tenants[i].addr,
				bb.arbitrator.addr, token, prop, 1, 65535, 14*86400, 14*86400, 30*86400), registry)
			p, h, err := bb.send(ctx, bb.deployer, nil, append(append([]byte{}, bb.art["RentalAgreement"].Bytecode...), ctor...), 5_000_000)
			if err != nil {
				return err
			}
			pend, hashes = append(pend, p), append(hashes, h)
			p2, h2, err := bb.send(ctx, bb.deployer, &token, bb.call("PlainStablecoin", "mint", bb.tenants[i].addr, new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), 200_000)
			if err != nil {
				return err
			}
			pend, hashes = append(pend, p2), append(hashes, h2)
		}
		for j, p := range pend {
			if ok, _, reason := p.Wait(ctx); !ok {
				return fmt.Errorf("setup tx: %s", reason)
			}
			r, err := bb.ec.TransactionReceipt(ctx, hashes[j])
			if err != nil || r.Status != 1 {
				return fmt.Errorf("setup tx failed: %v", err)
			}
			if j%2 == 0 {
				leases[start+j/2] = r.ContractAddress
			}
		}
		fmt.Printf("deployed %d/%d leases\n", end, n)
	}
	err = parallel(n, 64, func(i int) error {
		lease := leases[i]
		prop := crypto.Keccak256Hash([]byte(fmt.Sprintf("did:example:property:%d", i)))
		la, lsig := bb.attest(registry, bb.landlords[i].addr, fmt.Sprintf("did:example:landlord-%d", i), 1, prop)
		ta, tsig := bb.attest(registry, bb.tenants[i].addr, fmt.Sprintf("did:example:tenant-%d", i), 2, [32]byte{})
		if _, err := bb.exec(ctx, bb.landlords[i], &lease, bb.call("RentalAgreement", "bindIdentity", la, lsig), 300_000, "bind landlord"); err != nil {
			return fmt.Errorf("lease %d: %w", i, err)
		}
		if _, err := bb.exec(ctx, bb.tenants[i], &lease, bb.call("RentalAgreement", "bindIdentity", ta, tsig), 300_000, "bind tenant"); err != nil {
			return fmt.Errorf("lease %d: %w", i, err)
		}
		maxU := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
		if _, err := bb.exec(ctx, bb.tenants[i], &token, bb.call("PlainStablecoin", "approve", lease, maxU), 100_000, "approve"); err != nil {
			return fmt.Errorf("lease %d: %w", i, err)
		}
		if _, err := bb.exec(ctx, bb.tenants[i], &lease, bb.call("RentalAgreement", "lockDeposit"), 300_000, "lockDeposit"); err != nil {
			return fmt.Errorf("lease %d: %w", i, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i, l := range leases {
		st.Leases[i] = l.Hex()
	}
	bb.st = st
	return saveJSON(stateFile, st)
}

// Load restores state and re-reads tenant nonces from the node.
func (bb *BesuBackend) Load(ctx context.Context, stateFile string) error {
	st := &besuState{}
	if err := loadJSON(stateFile, st); err != nil {
		return err
	}
	bb.st = st
	bb.accounts(st.Seed, len(st.Leases))
	for _, a := range append(append([]*account{bb.deployer, bb.issuer, bb.arbitrator}, bb.landlords...), bb.tenants...) {
		nonce, err := bb.ec.PendingNonceAt(ctx, a.addr)
		if err != nil {
			return err
		}
		a.nonce = nonce
	}
	return nil
}

// Submit implements Backend: tenant i pays rent.
func (bb *BesuBackend) Submit(ctx context.Context, lease int) (Pending, error) {
	to := common.HexToAddress(bb.st.Leases[lease])
	p, _, err := bb.send(ctx, bb.tenants[lease], &to, bb.call("RentalAgreement", "payRent"), 200_000)
	return p, err
}

// ------------------------------------------------------------------ block watcher
// On every new head the watcher fetches the block's receipts in one call (eth_getBlockReceipts)
// and resolves the pending transactions it contains with their execution status. The commit
// time is the moment the head notification reached the client.
type inclusion struct {
	at time.Time
	ok bool
}

type blockWatcher struct {
	mu      sync.Mutex
	pending map[common.Hash]chan inclusion
	sub     *rpc.ClientSubscription
	rc      *rpc.Client
	quit    chan struct{}
}

type chanPending struct {
	ch chan inclusion
	w  *blockWatcher
	h  common.Hash
}

func (p *chanPending) Wait(ctx context.Context) (bool, time.Time, string) {
	select {
	case in := <-p.ch:
		if !in.ok {
			return false, in.at, "reverted"
		}
		return true, in.at, ""
	case <-ctx.Done():
		p.w.forget(p.h)
		return false, time.Time{}, "timeout"
	}
}

func startBlockWatcher(ws, rc *rpc.Client) (*blockWatcher, error) {
	w := &blockWatcher{pending: map[common.Hash]chan inclusion{}, rc: rc, quit: make(chan struct{})}
	heads := make(chan *types.Header, 1024)
	sub, err := ws.EthSubscribe(context.Background(), heads, "newHeads")
	if err != nil {
		return nil, err
	}
	w.sub = sub
	go func() {
		for {
			select {
			case <-w.quit:
				return
			case h := <-heads:
				seen := time.Now()
				var receipts []struct {
					TxHash common.Hash    `json:"transactionHash"`
					Status hexutil.Uint64 `json:"status"`
				}
				for attempt := 0; attempt < 5; attempt++ {
					if err := rc.CallContext(context.Background(), &receipts, "eth_getBlockReceipts", hexutil.EncodeBig(h.Number)); err == nil {
						break
					}
					time.Sleep(200 * time.Millisecond)
				}
				w.mu.Lock()
				for _, r := range receipts {
					if ch, ok := w.pending[r.TxHash]; ok {
						ch <- inclusion{at: seen, ok: r.Status == 1}
						delete(w.pending, r.TxHash)
					}
				}
				w.mu.Unlock()
			}
		}
	}()
	return w, nil
}

func (w *blockWatcher) expect(h common.Hash) Pending {
	ch := make(chan inclusion, 1)
	w.mu.Lock()
	w.pending[h] = ch
	w.mu.Unlock()
	return &chanPending{ch: ch, w: w, h: h}
}

func (w *blockWatcher) forget(h common.Hash) {
	w.mu.Lock()
	delete(w.pending, h)
	w.mu.Unlock()
}

func (w *blockWatcher) stop() { close(w.quit); w.sub.Unsubscribe() }
