package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/store"
)

// Output goes to stdout; logs go to stderr. That split is what lets
// `soroforge list --json | jq` work while logging is still on.
//
// Nothing printed here includes key material. Results carry the deployer's
// public address and never the seed that produced it.

func out() io.Writer { return os.Stdout }

// printJSON writes an indented JSON document.
func printJSON(v any) error {
	enc := json.NewEncoder(out())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// shortHash abbreviates a 64-character hash for table output. The full value is
// always available with --json, so the table optimises for scanning.
func shortHash(hash string) string {
	const shown = 12
	if len(hash) <= shown {
		return hash
	}
	return hash[:shown] + "…"
}

func printDeployResult(r *deploy.DeployResult) error {
	if flags.jsonOutput {
		return printJSON(r)
	}

	w := out()
	if r.DryRun {
		fmt.Fprintf(w, "Dry run — nothing was submitted and nothing was recorded.\n\n")
	} else {
		fmt.Fprintf(w, "Deployed %s to %s.\n\n", r.Alias, r.Network)
	}

	fmt.Fprintf(w, "  Contract ID   %s\n", r.ContractID)
	fmt.Fprintf(w, "  WASM hash     %s\n", r.WasmHash)
	fmt.Fprintf(w, "  Network       %s\n", r.Network)
	fmt.Fprintf(w, "  Deployer      %s\n", r.Deployer)
	if r.ConstructorArgs != "" {
		fmt.Fprintf(w, "  Constructor   %s\n", r.ConstructorArgs)
	}

	if r.UploadSkipped {
		fmt.Fprintf(w, "  Upload        skipped (this bytecode is already on-chain)\n")
	} else if r.UploadTxHash != "" {
		fmt.Fprintf(w, "  Upload tx     %s\n", r.UploadTxHash)
	}
	if r.CreateTxHash != "" {
		fmt.Fprintf(w, "  Create tx     %s\n", r.CreateTxHash)
	}
	if r.Ledger != 0 {
		fmt.Fprintf(w, "  Ledger        %d\n", r.Ledger)
	}
	printCatalog(w, r.Catalog)

	if r.DryRun {
		fmt.Fprintf(w, "\nAssembled transaction envelopes (unsigned):\n")
		if r.UploadEnvelopeXDR != "" {
			fmt.Fprintf(w, "\n  upload:\n  %s\n", r.UploadEnvelopeXDR)
		}
		if r.CreateEnvelopeXDR != "" {
			fmt.Fprintf(w, "\n  create:\n  %s\n", r.CreateEnvelopeXDR)
		}
		fmt.Fprintf(w, "\nRe-run without --dry-run to submit.\n")
	}

	return nil
}

func printUpgradeResult(r *deploy.UpgradeResult) error {
	if flags.jsonOutput {
		return printJSON(r)
	}

	w := out()

	if r.NoChange {
		fmt.Fprintf(w, "%s on %s already runs this WASM hash — nothing to do.\n\n",
			r.Alias, r.Network)
		fmt.Fprintf(w, "  Contract ID   %s\n", r.ContractID)
		fmt.Fprintf(w, "  WASM hash     %s\n", r.WasmHash)
		return nil
	}

	if r.DryRun {
		fmt.Fprintf(w, "Dry run — nothing was submitted and nothing was recorded.\n\n")
	} else {
		fmt.Fprintf(w, "Upgraded %s on %s.\n\n", r.Alias, r.Network)
	}

	fmt.Fprintf(w, "  Contract ID   %s\n", r.ContractID)
	fmt.Fprintf(w, "  From          %s\n", r.PreviousWasmHash)
	fmt.Fprintf(w, "  To            %s\n", r.WasmHash)
	fmt.Fprintf(w, "  Entrypoint    %s\n", r.UpgradeFunc)
	fmt.Fprintf(w, "  Deployer      %s\n", r.Deployer)

	if r.UploadSkipped {
		fmt.Fprintf(w, "  Upload        skipped (this bytecode is already on-chain)\n")
	} else if r.UploadTxHash != "" {
		fmt.Fprintf(w, "  Upload tx     %s\n", r.UploadTxHash)
	}
	if r.UpgradeTxHash != "" {
		fmt.Fprintf(w, "  Upgrade tx    %s\n", r.UpgradeTxHash)
	}
	if r.Ledger != 0 {
		fmt.Fprintf(w, "  Ledger        %d\n", r.Ledger)
	}
	printCatalog(w, r.Catalog)

	if r.DryRun && r.UpgradeEnvelopeXDR != "" {
		fmt.Fprintf(w, "\nAssembled upgrade envelope (unsigned):\n\n  %s\n", r.UpgradeEnvelopeXDR)
		fmt.Fprintf(w, "\nRe-run without --dry-run to submit.\n")
	}

	return nil
}

// printCatalog reports SoroVault registration. A failure is shown as a
// warning, not an error: the deploy itself succeeded and is recorded.
func printCatalog(w io.Writer, c *deploy.CatalogStatus) {
	if c == nil {
		return
	}
	if !c.OK {
		fmt.Fprintf(w, "  SoroVault     not registered: %s\n", c.Error)
		fmt.Fprintf(w, "                (the deploy succeeded; run `sorovault add` to catch up)\n")
		return
	}
	fmt.Fprintf(w, "  SoroVault     %s (%d functions)\n", c.Entry.URL, c.Entry.Functions)
}

// printDeployAll reports one batch deploy. It always prints every result —
// including a failure — because the summary is how the caller learns what
// succeeded before the run stopped.
func printDeployAll(results []deploy.DeployAllResult) error {
	if flags.jsonOutput {
		return printJSON(results)
	}

	w := out()
	if len(results) == 0 {
		fmt.Fprintln(w, "No contracts were deployed.")
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ALIAS\tSTATUS\tCONTRACT ID\tWASM HASH\tDETAIL")
	for _, r := range results {
		contractID, wasmHash, detail := "-", "-", ""
		if r.Result != nil {
			contractID = r.Result.ContractID
			wasmHash = shortHash(r.Result.WasmHash)
			if r.Result.DryRun {
				detail = "nothing submitted"
			} else if r.Result.UploadSkipped {
				detail = "upload skipped"
			}
		}
		switch r.Status {
		case deploy.DeployAllSkipped:
			detail = "already tracked"
		case deploy.DeployAllFailed:
			detail = r.Error
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Alias, r.Status, contractID, wasmHash, detail)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	counts := map[string]int{}
	for _, r := range results {
		counts[r.Status]++
	}
	fmt.Fprintf(w, "\n%d deployed, %d simulated, %d skipped, %d failed.\n",
		counts[deploy.DeployAllDeployed],
		counts[deploy.DeployAllDryRun],
		counts[deploy.DeployAllSkipped],
		counts[deploy.DeployAllFailed])
	if last := results[len(results)-1]; last.Status == deploy.DeployAllFailed {
		fmt.Fprintf(w, "Stopped at %s; nothing after it was attempted.\n", last.Alias)
	}
	return nil
}

func printContracts(contracts []store.Contract) error {
	if flags.jsonOutput {
		return printJSON(map[string]any{"contracts": contracts})
	}

	if len(contracts) == 0 {
		fmt.Fprintln(out(), "No contracts tracked yet. Run `soroforge deploy <alias>` to add one.")
		return nil
	}

	tw := tabwriter.NewWriter(out(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NETWORK\tALIAS\tCONTRACT ID\tWASM HASH\tDEPLOYED")
	for _, c := range contracts {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			c.Network, c.Alias, c.ContractID, shortHash(c.CurrentWasmHash), formatTime(c.UpdatedAt))
	}
	return tw.Flush()
}

func printHistory(alias, network string, deployments []store.Deployment) error {
	if flags.jsonOutput {
		return printJSON(map[string]any{
			"alias":       alias,
			"network":     network,
			"deployments": deployments,
		})
	}

	if len(deployments) == 0 {
		fmt.Fprintf(out(), "No history for %s on %s.\n", alias, network)
		return nil
	}

	fmt.Fprintf(out(), "History for %s on %s (newest first):\n\n", alias, network)

	tw := tabwriter.NewWriter(out(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WHEN\tACTION\tWASM HASH\tDEPLOYER\tLEDGER\tTX\tNOTES")
	for _, d := range deployments {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			formatTime(d.CreatedAt),
			d.Action,
			shortHash(d.WasmHash),
			shortHash(d.DeployerPubkey),
			formatLedger(d.Ledger),
			shortHash(d.TxHash),
			truncate(d.Notes, 40),
		)
	}
	return tw.Flush()
}

func printStatus(r *deploy.StatusResult) error {
	if flags.jsonOutput {
		return printJSON(r)
	}

	w := out()
	fmt.Fprintf(w, "%s\n\n", r.Detail)

	fmt.Fprintf(w, "  Alias         %s\n", r.Alias)
	fmt.Fprintf(w, "  Network       %s\n", r.Network)
	if r.ContractID != "" {
		fmt.Fprintf(w, "  Contract ID   %s\n", r.ContractID)
	}
	fmt.Fprintf(w, "  State         %s\n", r.State)
	if r.ExpectedWasmHash != "" {
		fmt.Fprintf(w, "  Recorded      %s\n", r.ExpectedWasmHash)
	}
	if r.ActualWasmHash != "" {
		fmt.Fprintf(w, "  On-chain      %s\n", r.ActualWasmHash)
	}

	return nil
}

func printNetworkStatus(r *deploy.NetworkStatus) error {
	if flags.jsonOutput {
		return printJSON(r)
	}

	w := out()
	if len(r.Contracts) == 0 {
		fmt.Fprintf(w, "No contracts tracked on %s.\n", r.Network)
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ALIAS\tSTATE\tCONTRACT ID\tRECORDED\tON-CHAIN")
	for _, c := range r.Contracts {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			c.Alias, c.State, c.ContractID, shortHash(c.ExpectedWasmHash), shortHash(c.ActualWasmHash))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	synced := 0
	for _, c := range r.Contracts {
		if c.InSync() {
			synced++
		}
	}
	fmt.Fprintf(w, "\n%d of %d contract(s) on %s in sync.\n", synced, len(r.Contracts), r.Network)
	return nil
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

func formatLedger(ledger uint32) string {
	if ledger == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", ledger)
}

func truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if s == "" {
		return "-"
	}
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
