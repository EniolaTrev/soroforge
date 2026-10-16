package deploy_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/soroworks/soroforge/internal/catalog"
	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file exercises the full deploy/upgrade/status flow against a mocked RPC
// and an in-memory store. Nothing here touches a network or a database, which
// is the point: the real-network path is the same code, differing only in which
// Client implementation is injected.

const testWasm = "\x00asm\x01\x00\x00\x00" // a minimal WASM header

// harness bundles a Service with the fakes behind it, so tests can assert on
// what SoroForge did rather than only on what it returned.
type harness struct {
	svc    *deploy.Service
	client *stellar.MockClient
	signer *stellar.MockSigner
	store  *store.MemoryStore
	cfg    *config.Config

	// contractID is the address the mocked create simulation returns.
	contractID string
	wasmHash   xdr.Hash
}

type harnessOptions struct {
	// wasm overrides the contract bytecode, so tests can produce a different
	// hash for an upgrade.
	wasm string

	// configBody overrides the whole soroforge.yaml.
	configBody string

	// noSigner builds a read-only service.
	noSigner bool

	// catalog replaces the SoroVault registrar.
	catalog catalog.Registrar

	// upgradeNoop makes an accepted upgrade invocation leave the contract's
	// bytecode untouched, as a buggy contract or a custom upgrade entrypoint
	// that does something else would. newHarness otherwise keeps the mocked
	// ledger truthful by applying the hash the invocation passed.
	upgradeNoop bool

	// verifyDelay overrides the wait between post-upgrade hash reads, so tests
	// that exercise the retry do not sleep for seconds.
	verifyDelay time.Duration

	// log receives service logs. Tests that assert on what SoroForge reports
	// (a skipped contract, for example) capture them here instead of
	// discarding them.
	log io.Writer
}

func newHarness(t *testing.T, opts harnessOptions) *harness {
	t.Helper()

	wasm := opts.wasm
	if wasm == "" {
		wasm = testWasm
	}

	dir := t.TempDir()
	wasmPath := filepath.Join(dir, "counter.wasm")
	require.NoError(t, os.WriteFile(wasmPath, []byte(wasm), 0o600))

	body := opts.configBody
	if body == "" {
		body = `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
  mainnet:
    rpc_url: https://mainnet.example.org
    passphrase: "Public Global Stellar Network ; September 2015"
contracts:
  counter:
    wasm: ./counter.wasm
`
	}
	configPath := filepath.Join(dir, "soroforge.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(body), 0o600))

	cfg, err := config.Load(configPath)
	require.NoError(t, err)

	contractID := testContractID(t)
	client := stellar.NewMockClient()
	client.SimulateResponse = simulationReturning(t, contractID)
	if !opts.upgradeNoop {
		installUpgradeEffect(t, client)
	}

	memStore := store.NewMemoryStore()

	var signer stellar.Signer
	mockSigner := stellar.NewMockSigner()
	if !opts.noSigner {
		signer = mockSigner
	}

	// Discard logs so test output stays readable, unless the test wants them.
	logWriter := opts.log
	if logWriter == nil {
		logWriter = io.Discard
	}

	svc, err := deploy.New(deploy.Options{
		Config:  cfg,
		Store:   memStore,
		Signer:  signer,
		Clients: func(string, config.Network) (stellar.Client, error) { return client, nil },
		Log:     slog.New(slog.NewTextHandler(logWriter, nil)),

		Catalog:     opts.catalog,
		VerifyDelay: opts.verifyDelay,
	})
	require.NoError(t, err)

	return &harness{
		svc:        svc,
		client:     client,
		signer:     mockSigner,
		store:      memStore,
		cfg:        cfg,
		contractID: contractID,
		wasmHash:   stellar.WasmHash([]byte(wasm)),
	}
}

// installUpgradeEffect makes the mocked ledger apply what an accepted upgrade
// transaction would do: point the invoked contract's instance at the WASM hash
// the invocation passed.
//
// The read-back verification added to Upgrade makes this the default for the
// harness; without it, every upgrade test would see the old hash forever. A
// test that needs the buggy behaviour sets harnessOptions.upgradeNoop.
func installUpgradeEffect(t *testing.T, client *stellar.MockClient) {
	t.Helper()

	client.AfterSend = func(_ context.Context, signedTxXDR string) {
		var envelope xdr.TransactionEnvelope
		require.NoError(t, xdr.SafeUnmarshalBase64(signedTxXDR, &envelope), "sent envelope must be valid XDR")
		if envelope.V1 == nil {
			return
		}

		for _, op := range envelope.V1.Tx.Operations {
			invoke := op.Body.InvokeHostFunctionOp
			if invoke == nil ||
				invoke.HostFunction.Type != xdr.HostFunctionTypeHostFunctionTypeInvokeContract {
				continue
			}
			call := invoke.HostFunction.InvokeContract
			if call == nil || len(call.Args) == 0 || call.Args[0].Bytes == nil {
				continue
			}
			if len(*call.Args[0].Bytes) != len(xdr.Hash{}) {
				continue
			}

			address := call.ContractAddress
			contractID, err := stellar.ContractIDFromScVal(xdr.ScVal{
				Type:    xdr.ScValTypeScvAddress,
				Address: &address,
			})
			require.NoError(t, err)

			var hash xdr.Hash
			copy(hash[:], *call.Args[0].Bytes)
			require.NoError(t, client.SetContractInstance(contractID, hash))
		}
	}
}

// simulationReturning builds a simulation response whose return value is the
// given contract address, as a real create-contract simulation would produce.
func simulationReturning(t *testing.T, contractID string) stellar.SimulateResult {
	t.Helper()

	addr, err := stellar.ContractAddress(contractID)
	require.NoError(t, err)
	returnValue, err := xdr.MarshalBase64(xdr.ScVal{
		Type:    xdr.ScValTypeScvAddress,
		Address: &addr,
	})
	require.NoError(t, err)

	data, err := xdr.MarshalBase64(xdr.SorobanTransactionData{
		Resources: xdr.SorobanResources{
			Instructions:  xdr.Uint32(1_000_000),
			DiskReadBytes: xdr.Uint32(500),
			WriteBytes:    xdr.Uint32(1_000),
		},
		ResourceFee: xdr.Int64(12_345),
	})
	require.NoError(t, err)

	return stellar.SimulateResult{
		TransactionDataXDR: data,
		MinResourceFee:     12_345,
		Results:            []stellar.HostFunctionResult{{ReturnValueXDR: returnValue}},
		LatestLedger:       1000,
	}
}

func testContractID(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	id, err := strkey.Encode(strkey.VersionByteContract, raw)
	require.NoError(t, err)
	return id
}

// otherContractID returns a valid contract address distinct from
// testContractID and from other seeds.
func otherContractID(t *testing.T, seed byte) string {
	t.Helper()
	raw := make([]byte, 32)
	raw[0] = 0xF0
	raw[1] = seed
	id, err := strkey.Encode(strkey.VersionByteContract, raw)
	require.NoError(t, err)
	return id
}

func TestDeployRecordsContractAndHistory(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	result, err := h.svc.Deploy(ctx, deploy.DeployRequest{
		Alias: "counter",
		Notes: "first release",
	})
	require.NoError(t, err)

	assert.Equal(t, h.contractID, result.ContractID)
	assert.Equal(t, stellar.HashHex(h.wasmHash), result.WasmHash)
	assert.Equal(t, h.signer.Address(), result.Deployer)
	assert.False(t, result.DryRun)

	// Deploying is two transactions: upload the bytecode, then instantiate it.
	assert.Equal(t, 2, h.client.SendCount())

	// The contract must now be tracked.
	tracked, err := h.store.GetContract(ctx, "testnet", "counter")
	require.NoError(t, err)
	assert.Equal(t, h.contractID, tracked.ContractID)
	assert.Equal(t, stellar.HashHex(h.wasmHash), tracked.CurrentWasmHash)

	// And the history must record who did it and when.
	history, err := h.store.History(ctx, "testnet", "counter")
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, store.ActionDeploy, history[0].Action)
	assert.Equal(t, h.signer.Address(), history[0].DeployerPubkey)
	assert.Equal(t, "first release", history[0].Notes)
	assert.NotEmpty(t, history[0].TxHash)
}

func TestDeployDryRunSubmitsNothingAndRecordsNothing(t *testing.T) {
	// This is the guarantee that makes --dry-run safe to point at mainnet.
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	result, err := h.svc.Deploy(ctx, deploy.DeployRequest{Alias: "counter", DryRun: true})
	require.NoError(t, err)

	assert.True(t, result.DryRun)
	assert.Zero(t, h.client.SendCount(), "a dry run must not submit anything")
	assert.Zero(t, h.signer.SignCount(), "a dry run must not sign anything")

	// Nothing recorded.
	_, err = h.store.GetContract(ctx, "testnet", "counter")
	assert.ErrorIs(t, err, store.ErrNotFound)

	history, err := h.store.History(ctx, "testnet", "counter")
	require.NoError(t, err)
	assert.Empty(t, history)

	// But it still simulated, and it still produced assembled envelopes and the
	// address a real deploy would create — that is what makes it useful.
	assert.Equal(t, 2, h.client.SimulateCount())
	assert.NotEmpty(t, result.UploadEnvelopeXDR)
	assert.NotEmpty(t, result.CreateEnvelopeXDR)
	assert.Equal(t, h.contractID, result.ContractID)
	assert.Empty(t, result.UploadTxHash)
	assert.Empty(t, result.CreateTxHash)
}

func TestDeployDryRunEnvelopesAreValidXDR(t *testing.T) {
	// The envelopes are the deliverable of a dry run, so they must decode into
	// real transactions that another tool could inspect or submit.
	h := newHarness(t, harnessOptions{})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter", DryRun: true})
	require.NoError(t, err)

	for name, envelope := range map[string]string{
		"upload": result.UploadEnvelopeXDR,
		"create": result.CreateEnvelopeXDR,
	} {
		t.Run(name, func(t *testing.T) {
			var decoded xdr.TransactionEnvelope
			require.NoError(t, xdr.SafeUnmarshalBase64(envelope, &decoded))

			require.NotNil(t, decoded.V1)
			// Assembled means the Soroban resource data is attached.
			assert.Equal(t, int32(1), decoded.V1.Tx.Ext.V)
			require.NotNil(t, decoded.V1.Tx.Ext.SorobanData)
			assert.Equal(t, xdr.Int64(12_345), decoded.V1.Tx.Ext.SorobanData.ResourceFee)
			// Unsigned, because a dry run stops before signing.
			assert.Empty(t, decoded.Signatures())
		})
	}
}

func TestDeploySkipsUploadWhenBytecodeIsAlreadyOnChain(t *testing.T) {
	// Re-uploading identical bytecode is a no-op on-chain but still costs a
	// transaction and a fee.
	h := newHarness(t, harnessOptions{})
	require.NoError(t, h.client.SetWasmUploaded(h.wasmHash))

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	require.NoError(t, err)

	assert.True(t, result.UploadSkipped)
	assert.Empty(t, result.UploadTxHash)
	assert.Equal(t, 1, h.client.SendCount(), "only the create transaction should be submitted")
}

func TestDeployUsesRequestedNetwork(t *testing.T) {
	// Every record is namespaced by network; deploying to mainnet must not
	// appear under testnet.
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	_, err := h.svc.Deploy(ctx, deploy.DeployRequest{Alias: "counter", Network: "mainnet"})
	require.NoError(t, err)

	_, err = h.store.GetContract(ctx, "mainnet", "counter")
	require.NoError(t, err)

	_, err = h.store.GetContract(ctx, "testnet", "counter")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeployRejectsUnknownAliasAndNetwork(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()

	_, err := h.svc.Deploy(ctx, deploy.DeployRequest{Alias: "nonexistent"})
	assert.ErrorContains(t, err, "unknown contract")

	_, err = h.svc.Deploy(ctx, deploy.DeployRequest{Alias: "counter", Network: "staging"})
	assert.ErrorContains(t, err, "unknown network")
}

func TestDeployWithoutSignerFailsClearly(t *testing.T) {
	h := newHarness(t, harnessOptions{noSigner: true})

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	assert.ErrorContains(t, err, "no signing key configured")
	assert.ErrorContains(t, err, config.EnvSecretKey)
}

func TestDeployReportsMissingWasmArtifact(t *testing.T) {
	// A missing build output is the most common first-run failure; the message
	// should say what to do about it.
	h := newHarness(t, harnessOptions{configBody: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./never-built.wasm
`})

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	assert.ErrorContains(t, err, "wasm artifact not found")
	assert.ErrorContains(t, err, "has the contract been built?")
}

func TestDeployPropagatesSimulationFailure(t *testing.T) {
	// A simulation error arrives inside a successful RPC response, so it must
	// be checked explicitly rather than assumed absent.
	h := newHarness(t, harnessOptions{})
	h.client.SimulateResponse = stellar.SimulateResult{Error: "HostError: invalid wasm"}

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	assert.ErrorContains(t, err, "simulation failed")
	assert.ErrorContains(t, err, "invalid wasm")
	assert.Zero(t, h.client.SendCount(), "nothing should be submitted after a failed simulation")
}

func TestDeployReportsFailedTransaction(t *testing.T) {
	// A FAILED transaction returns a nil error from polling — failure is a
	// valid terminal state — so the status must be checked, or SoroForge would
	// record a deploy that never happened.
	h := newHarness(t, harnessOptions{})
	h.client.AwaitResponse = stellar.TxResult{
		Status: stellar.TxStatusFailed,
		Hash:   "abc",
		Ledger: 42,
	}

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	assert.ErrorContains(t, err, "failed")

	history, err := h.store.History(context.Background(), "testnet", "counter")
	require.NoError(t, err)
	assert.Empty(t, history, "a failed transaction must not be recorded as a deployment")
}

func TestDeployReportsRejectedSubmission(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.client.SendFunc = func(context.Context, string) (stellar.SendResult, error) {
		return stellar.SendResult{Status: stellar.SendStatusError, ErrorResultXDR: "AAAAA"}, nil
	}

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	assert.ErrorContains(t, err, "rejected by the network")
}

func TestDeployRefusesWhenRestoreIsRequired(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	sim := h.client.SimulateResponse
	sim.RestoreRequired = true
	sim.RestoreTransactionDataXDR = sim.TransactionDataXDR
	h.client.SimulateResponse = sim

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter"})
	assert.ErrorContains(t, err, "restored")
	assert.Zero(t, h.client.SendCount())
}

func TestDeployUsesConfiguredSaltForDeterministicAddress(t *testing.T) {
	// A configured salt is what makes a contract address reproducible.
	const salt = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

	h := newHarness(t, harnessOptions{configBody: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./counter.wasm
    salt: "` + salt + `"
`})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter", DryRun: true})
	require.NoError(t, err)

	// Decode the create envelope and confirm the salt reached the preimage.
	var decoded xdr.TransactionEnvelope
	require.NoError(t, xdr.SafeUnmarshalBase64(result.CreateEnvelopeXDR, &decoded))

	op := decoded.V1.Tx.Operations[0].Body.MustInvokeHostFunctionOp()
	require.Equal(t, xdr.HostFunctionTypeHostFunctionTypeCreateContractV2, op.HostFunction.Type)

	expected, err := config.ParseSalt(salt)
	require.NoError(t, err)
	assert.Equal(t, xdr.Uint256(expected),
		op.HostFunction.CreateContractV2.ContractIdPreimage.FromAddress.Salt)
}

func TestDeployPassesConstructorArguments(t *testing.T) {
	h := newHarness(t, harnessOptions{configBody: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./counter.wasm
    constructor_args:
      - {type: u32, value: 7}
      - {type: string, value: "SoroForge"}
`})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{Alias: "counter", DryRun: true})
	require.NoError(t, err)

	var decoded xdr.TransactionEnvelope
	require.NoError(t, xdr.SafeUnmarshalBase64(result.CreateEnvelopeXDR, &decoded))

	op := decoded.V1.Tx.Operations[0].Body.MustInvokeHostFunctionOp()
	args := op.HostFunction.CreateContractV2.ConstructorArgs
	require.Len(t, args, 2)
	assert.Equal(t, xdr.Uint32(7), *args[0].U32)
	assert.Equal(t, xdr.ScString("SoroForge"), *args[1].Str)
}

func TestDeployRequestConstructorArgsOverrideTheConfig(t *testing.T) {
	// --arg and --args-file replace the config's constructor_args for one
	// deploy, so the same alias can be instantiated differently per
	// environment without editing shared config.
	h := newHarness(t, harnessOptions{configBody: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./counter.wasm
    constructor_args:
      - {type: u32, value: 1}
`})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{
		Alias: "counter",
		ConstructorArgs: []config.Arg{
			{Type: config.ArgU32, Value: config.NewValue("2")},
			{Type: config.ArgString, Value: config.NewValue("from the command line")},
		},
		DryRun: true,
	})
	require.NoError(t, err)

	var decoded xdr.TransactionEnvelope
	require.NoError(t, xdr.SafeUnmarshalBase64(result.CreateEnvelopeXDR, &decoded))

	op := decoded.V1.Tx.Operations[0].Body.MustInvokeHostFunctionOp()
	args := op.HostFunction.CreateContractV2.ConstructorArgs
	require.Len(t, args, 2, "the override replaces the config value entirely")
	assert.Equal(t, xdr.Uint32(2), *args[0].U32)
	assert.Equal(t, xdr.ScString("from the command line"), *args[1].Str)
}

func TestDeployRequestConstructorArgsCanBeEmpty(t *testing.T) {
	// A non-nil empty slice is an override: deploy with no arguments even
	// though the config has some.
	h := newHarness(t, harnessOptions{configBody: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./counter.wasm
    constructor_args:
      - {type: u32, value: 1}
`})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{
		Alias:           "counter",
		ConstructorArgs: []config.Arg{},
		DryRun:          true,
	})
	require.NoError(t, err)

	var decoded xdr.TransactionEnvelope
	require.NoError(t, xdr.SafeUnmarshalBase64(result.CreateEnvelopeXDR, &decoded))
	op := decoded.V1.Tx.Operations[0].Body.MustInvokeHostFunctionOp()
	assert.Empty(t, op.HostFunction.CreateContractV2.ConstructorArgs)
}

func TestDeployRecordsTheConstructorArgsUsed(t *testing.T) {
	// The arguments actually used are recorded with the deployment, so the
	// deploy can be reproduced after the config has moved on. They live in a
	// dedicated constructor_args column, not in the free-form notes.
	h := newHarness(t, harnessOptions{configBody: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
contracts:
  counter:
    wasm: ./counter.wasm
    constructor_args:
      - {type: u32, value: 7}
      - {type: string, value: "SoroForge"}
`})

	// Built the way the CLI builds it, so this covers the --arg path end to
	// end (--arg u32:9).
	arg, err := config.ParseArgFlag("u32:9")
	require.NoError(t, err)

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{
		Alias:           "counter",
		Notes:           "release 1.2.3",
		ConstructorArgs: []config.Arg{arg},
	})
	require.NoError(t, err)
	assert.Equal(t, `[{"type":"u32","value":9}]`, result.ConstructorArgs)

	history, err := h.store.History(context.Background(), "testnet", "counter")
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, `[{"type":"u32","value":9}]`, history[0].ConstructorArgs,
		"the recorded args must be the ones actually used, in typed JSON form")
	assert.Equal(t, "release 1.2.3", history[0].Notes, "notes stay free-form")
}

func TestDeployRejectsInvalidRequestConstructorArgs(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	_, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{
		Alias: "counter",
		ConstructorArgs: []config.Arg{
			{Type: config.ArgU32, Value: config.NewValue("not a number")},
		},
	})
	assert.ErrorContains(t, err, "constructor args for counter")
	assert.ErrorContains(t, err, "arg[0]")
	assert.Zero(t, h.client.SendCount(), "a bad argument must not submit anything")
}

func TestDeployAppliesPerNetworkOverrides(t *testing.T) {
	// The override resolution is shared by deploy, dry run, and status through
	// Service.resolve, so a deploy to mainnet must use mainnet's arguments and
	// salt while testnet keeps the contract defaults.
	const saltMainnet = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
	h := newHarness(t, harnessOptions{configBody: `
version: 1
default_network: testnet
networks:
  testnet:
    rpc_url: https://rpc.example.org
    passphrase: "Test SDF Network ; September 2015"
  mainnet:
    rpc_url: https://mainnet.example.org
    passphrase: "Public Global Stellar Network ; September 2015"
contracts:
  counter:
    wasm: ./counter.wasm
    constructor_args:
      - {type: u32, value: 1}
    networks:
      mainnet:
        constructor_args:
          - {type: u32, value: 2}
        salt: "` + saltMainnet + `"
`})

	result, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{
		Alias:   "counter",
		Network: "mainnet",
		DryRun:  true,
	})
	require.NoError(t, err)

	var decoded xdr.TransactionEnvelope
	require.NoError(t, xdr.SafeUnmarshalBase64(result.CreateEnvelopeXDR, &decoded))
	op := decoded.V1.Tx.Operations[0].Body.MustInvokeHostFunctionOp()
	require.Len(t, op.HostFunction.CreateContractV2.ConstructorArgs, 1)
	assert.Equal(t, xdr.Uint32(2), *op.HostFunction.CreateContractV2.ConstructorArgs[0].U32)

	expectedSalt, err := config.ParseSalt(saltMainnet)
	require.NoError(t, err)
	assert.Equal(t, xdr.Uint256(expectedSalt),
		op.HostFunction.CreateContractV2.ContractIdPreimage.FromAddress.Salt)

	// Testnet has no override, so it uses the contract's own arguments.
	testnet, err := h.svc.Deploy(context.Background(), deploy.DeployRequest{
		Alias:  "counter",
		DryRun: true,
	})
	require.NoError(t, err)

	require.NoError(t, xdr.SafeUnmarshalBase64(testnet.CreateEnvelopeXDR, &decoded))
	op = decoded.V1.Tx.Operations[0].Body.MustInvokeHostFunctionOp()
	require.Len(t, op.HostFunction.CreateContractV2.ConstructorArgs, 1)
	assert.Equal(t, xdr.Uint32(1), *op.HostFunction.CreateContractV2.ConstructorArgs[0].U32)
}
