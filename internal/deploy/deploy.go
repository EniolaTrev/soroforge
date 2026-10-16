package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
)

// DeployRequest asks for a contract to be deployed.
type DeployRequest struct {
	// Alias names a contract in soroforge.yaml.
	Alias string `json:"alias"`

	// Network names a network in soroforge.yaml. Empty uses default_network.
	Network string `json:"network,omitempty"`

	// Notes is free-form context recorded with the deployment.
	Notes string `json:"notes,omitempty"`

	// DryRun assembles and simulates the transactions but submits nothing and
	// records nothing.
	DryRun bool `json:"dry_run,omitempty"`

	// ConstructorArgs overrides the contract's configured constructor_args
	// for this one deploy. A non-nil slice — including an empty one — replaces
	// the resolved config value entirely; nil keeps it. The CLI sets this from
	// --arg or --args-file. It is deliberately absent from the HTTP API.
	ConstructorArgs []config.Arg `json:"-"`
}

// DeployResult describes a completed or simulated deploy.
type DeployResult struct {
	Alias      string `json:"alias"`
	Network    string `json:"network"`
	ContractID string `json:"contract_id"`
	WasmHash   string `json:"wasm_hash"`
	Deployer   string `json:"deployer_pubkey"`

	// UploadTxHash and CreateTxHash are the two transactions a deploy performs.
	// Both are empty on a dry run.
	UploadTxHash string `json:"upload_tx_hash,omitempty"`
	CreateTxHash string `json:"create_tx_hash,omitempty"`

	Ledger uint32 `json:"ledger,omitempty"`

	// DryRun reports whether anything was actually submitted.
	DryRun bool `json:"dry_run"`

	// UploadEnvelopeXDR and CreateEnvelopeXDR are the assembled transaction
	// envelopes. On a dry run these are the deliverable: they can be inspected,
	// diffed, or submitted by another tool.
	UploadEnvelopeXDR string `json:"upload_envelope_xdr,omitempty"`
	CreateEnvelopeXDR string `json:"create_envelope_xdr,omitempty"`

	// UploadSkipped reports that the bytecode was already on-chain.
	UploadSkipped bool `json:"upload_skipped,omitempty"`

	// ConstructorArgs records the typed arguments this deploy actually used,
	// after per-network overrides and any CLI replacement, as JSON.
	ConstructorArgs string `json:"constructor_args,omitempty"`

	// Deployment is the recorded history entry, absent on a dry run.
	Deployment *store.Deployment `json:"deployment,omitempty"`

	// Catalog reports registration with the network's sorovault_url. Absent
	// on a dry run or when no registry is configured.
	Catalog *CatalogStatus `json:"catalog,omitempty"`
}

// Deploy uploads a contract's bytecode and instantiates it, then records the
// result.
//
// Deploying is two transactions because Soroban permits one host function per
// transaction: first the bytecode is uploaded and becomes addressable by its
// hash, then a contract instance is created from that hash. The instance is
// what gets an address; the bytecode is shared by every contract using it.
func (s *Service) Deploy(ctx context.Context, req DeployRequest) (*DeployResult, error) {
	r, err := s.resolve(req.Network, req.Alias)
	if err != nil {
		return nil, err
	}
	signer, err := s.requireSigner()
	if err != nil {
		return nil, err
	}

	wasm, err := s.readWasm(r.Contract)
	if err != nil {
		return nil, err
	}
	wasmHash := stellar.WasmHash(wasm)
	wasmHashHex := stellar.HashHex(wasmHash)

	log := s.log.With(
		"alias", r.Alias,
		"network", r.NetworkName,
		"wasm_hash", wasmHashHex,
		"dry_run", req.DryRun,
	)
	log.Info("deploying contract", "deployer", signer.Address())

	result := &DeployResult{
		Alias:    r.Alias,
		Network:  r.NetworkName,
		WasmHash: wasmHashHex,
		Deployer: signer.Address(),
		DryRun:   req.DryRun,
	}

	// Resolve the constructor arguments up front: a bad argument should fail
	// before either transaction is submitted, not after the bytecode upload
	// has already paid for itself.
	//
	// The arguments resolved here are the ones actually used: the CLI
	// replacement if there is one, otherwise the contract's constructor_args
	// with this network's override already applied by resolve.
	contractArgs := r.Contract.ConstructorArgs
	if req.ConstructorArgs != nil {
		contractArgs = req.ConstructorArgs
	}
	args, err := config.ArgsToScVals(contractArgs)
	if err != nil {
		return nil, fmt.Errorf("constructor args for %s: %w", r.Alias, err)
	}
	argsJSON, err := config.ArgsToJSON(contractArgs)
	if err != nil {
		return nil, err
	}
	result.ConstructorArgs = argsJSON

	// Step 1: upload the bytecode, unless it is already on-chain. Uploading is
	// content-addressed, so an identical upload is a no-op that still costs a
	// transaction and a fee.
	uploaded, err := stellar.WasmUploaded(ctx, r.Client, wasmHash)
	if err != nil {
		return nil, fmt.Errorf("check whether wasm is already uploaded: %w", err)
	}
	if uploaded {
		log.Info("wasm already on-chain, skipping upload")
		result.UploadSkipped = true
	} else {
		upload, err := s.runTransaction(ctx, r,
			stellar.UploadWasmOp(wasm, signer.Address()), req.DryRun)
		if err != nil {
			return nil, fmt.Errorf("upload wasm: %w", err)
		}
		result.UploadTxHash = upload.Hash
		result.UploadEnvelopeXDR = upload.EnvelopeXDR
		log.Info("wasm uploaded", "tx_hash", upload.Hash)
	}

	// Step 2: instantiate a contract from the uploaded bytecode.
	salt, err := s.salt(r.Contract)
	if err != nil {
		return nil, err
	}

	createOp, err := stellar.CreateContractOp(wasmHash, signer.Address(), salt, args, signer.Address())
	if err != nil {
		return nil, err
	}

	create, err := s.runTransaction(ctx, r, createOp, req.DryRun)
	if err != nil {
		return nil, fmt.Errorf("create contract: %w", err)
	}
	result.CreateTxHash = create.Hash
	result.CreateEnvelopeXDR = create.EnvelopeXDR
	result.Ledger = create.Ledger

	// The contract's address comes from the host function's return value, which
	// simulation reports — so a dry run knows the address a real deploy would
	// produce.
	contractID, err := stellar.ContractIDFromSimulation(create.Simulation)
	if err != nil {
		return nil, fmt.Errorf("determine contract id: %w", err)
	}
	result.ContractID = contractID

	if req.DryRun {
		log.Info("dry run complete, nothing submitted or recorded", "contract_id", contractID)
		return result, nil
	}

	log.Info("contract created", "contract_id", contractID, "tx_hash", create.Hash)

	// Record only now, after the network has confirmed both transactions.
	deployment, err := s.record(ctx, store.Deployment{
		Alias:           r.Alias,
		Network:         r.NetworkName,
		ContractID:      contractID,
		WasmHash:        wasmHashHex,
		Action:          store.ActionDeploy,
		DeployerPubkey:  signer.Address(),
		TxHash:          create.Hash,
		Ledger:          create.Ledger,
		Notes:           req.Notes,
		ConstructorArgs: argsJSON,
	})
	if err != nil {
		return nil, err
	}
	result.Deployment = deployment
	result.Catalog = s.publish(ctx, r, contractID, log)

	return result, nil
}

// DeployAllRequest asks for every contract in soroforge.yaml that is not yet
// tracked on a network to be deployed in one run.
type DeployAllRequest struct {
	// Network names a network in soroforge.yaml. Empty uses default_network.
	Network string `json:"network,omitempty"`

	// Notes is free-form context recorded with every deployment in the run.
	Notes string `json:"notes,omitempty"`

	// DryRun assembles and simulates every deploy but submits and records
	// nothing.
	DryRun bool `json:"dry_run,omitempty"`
}

// Statuses reported for each contract in a DeployAll run.
const (
	// DeployAllDeployed means the contract was deployed and recorded.
	DeployAllDeployed = "deployed"

	// DeployAllDryRun means the deploy was simulated, nothing submitted.
	DeployAllDryRun = "dry_run"

	// DeployAllSkipped means the contract is already tracked on the network
	// and was left alone.
	DeployAllSkipped = "skipped"

	// DeployAllFailed means the contract's deploy failed, stopping the run.
	DeployAllFailed = "failed"
)

// DeployAllResult is one contract's outcome in a batch deploy.
type DeployAllResult struct {
	Alias string `json:"alias"`

	// Status is one of DeployAllDeployed, DeployAllDryRun, DeployAllSkipped,
	// or DeployAllFailed.
	Status string `json:"status"`

	// Result is the full deploy result for an attempted contract, the same
	// shape a single deploy returns. Absent for a skipped or failed contract.
	Result *DeployResult `json:"result,omitempty"`

	// Error explains why a contract failed. A failure stops the run.
	Error string `json:"error,omitempty"`
}

// DeployAll deploys every contract in the config that is not yet tracked on
// the network, in alias order, and skips the ones that are already tracked.
//
// A tracked contract is skipped rather than redeployed: deploying it again
// would create a second contract and silently move the alias to a new address.
//
// The first failure stops the run. The results gathered so far — including the
// failure itself — are returned alongside the error, so callers can report
// what succeeded before the run stopped.
//
// Contracts are processed in alias order, which does not resolve dependencies
// between them (a constructor argument that needs another contract's address
// cannot be discovered here); such projects need explicit ordering.
func (s *Service) DeployAll(ctx context.Context, req DeployAllRequest) ([]DeployAllResult, error) {
	name, _, err := s.cfg.Network(req.Network)
	if err != nil {
		return nil, err
	}

	aliases := make([]string, 0, len(s.cfg.Contracts))
	for alias := range s.cfg.Contracts {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)

	results := make([]DeployAllResult, 0, len(aliases))
	for _, alias := range aliases {
		_, err := s.store.GetContract(ctx, name, alias)
		switch {
		case err == nil:
			s.log.Info("contract is already tracked, skipping", "alias", alias, "network", name)
			results = append(results, DeployAllResult{Alias: alias, Status: DeployAllSkipped})
			continue
		case !errors.Is(err, store.ErrNotFound):
			return results, fmt.Errorf("check whether %s is tracked on %s: %w", alias, name, err)
		}

		result, err := s.Deploy(ctx, DeployRequest{
			Alias:   alias,
			Network: name,
			Notes:   req.Notes,
			DryRun:  req.DryRun,
		})
		if err != nil {
			results = append(results, DeployAllResult{
				Alias:  alias,
				Status: DeployAllFailed,
				Error:  err.Error(),
			})
			return results, fmt.Errorf("deploy %s on %s: %w", alias, name, err)
		}

		status := DeployAllDeployed
		if result.DryRun {
			status = DeployAllDryRun
		}
		results = append(results, DeployAllResult{Alias: alias, Status: status, Result: result})
	}
	return results, nil
}

// record writes a history entry and updates the contract's current state.
//
// If this fails the contract is already live on-chain, so the error says so
// explicitly: the deploy succeeded and only the bookkeeping did not, which is a
// recoverable situation but not one to discover from a bare database error.
func (s *Service) record(ctx context.Context, d store.Deployment) (*store.Deployment, error) {
	recorded, err := s.store.RecordDeployment(ctx, d)
	if err != nil {
		return nil, fmt.Errorf(
			"contract %s is live on %s (%s) but recording it failed: %w",
			d.Alias, d.Network, d.ContractID, err)
	}

	if _, err := s.store.UpsertContract(ctx, store.Contract{
		Alias:           d.Alias,
		Network:         d.Network,
		ContractID:      d.ContractID,
		CurrentWasmHash: d.WasmHash,
	}); err != nil {
		return nil, fmt.Errorf(
			"contract %s is live on %s (%s) and the history entry was written, "+
				"but updating its current state failed: %w",
			d.Alias, d.Network, d.ContractID, err)
	}

	return &recorded, nil
}

// readWasm loads a contract's compiled artifact.
func (s *Service) readWasm(c config.Contract) ([]byte, error) {
	path := s.cfg.WasmPath(c)
	wasm, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("wasm artifact not found at %s (has the contract been built?)", path)
		}
		return nil, fmt.Errorf("read wasm %s: %w", path, err)
	}
	if len(wasm) == 0 {
		return nil, fmt.Errorf("wasm artifact at %s is empty", path)
	}
	return wasm, nil
}

// salt returns the contract's configured salt, or a fresh random one.
func (s *Service) salt(c config.Contract) ([32]byte, error) {
	if c.Salt != "" {
		return config.ParseSalt(c.Salt)
	}
	return stellar.NewSalt()
}
