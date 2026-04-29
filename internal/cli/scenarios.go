package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/scenarios"
)

// newScenariosCommand wires the scenario catalog subcommands.
//
//	scenarios list                — list every built-in scenario
//	scenarios validate <file>     — validate a scenario YAML file
//	scenarios show <name>         — print the YAML of a built-in scenario
//	scenarios archetypes <name>   — print the archetype configs of a scenario
//
// `scenarios` with no subcommand prints help — Cobra default behavior on a
// parent command without RunE.
func newScenariosCommand(_ *GlobalFlags) *cobra.Command {
	root := &cobra.Command{
		Use:   "scenarios",
		Short: "List, describe, and validate built-in load-test scenarios",
		Long: `Inspect the built-in scenario catalog and validate user scenarios
against the schema before a run.

Built-in scenarios cover the Crawl-Walk-Run methodology plus a matrix
billing scenario (every pricing × billing combo) and a lifecycle-stress
profile. See "scenarios list" for the catalog.`,
	}
	root.AddCommand(
		newScenariosListCommand(),
		newScenariosValidateCommand(),
		newScenariosShowCommand(),
		newScenariosArchetypesCommand(),
	)
	return root
}

func newScenariosListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List built-in scenarios with name, target TPS, and duration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runScenariosList(cmd.OutOrStdout())
		},
	}
}

func newScenariosValidateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <file>",
		Short: "Validate a scenario YAML file against the schema",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScenariosValidate(cmd.OutOrStdout(), cmd.ErrOrStderr(), args[0])
		},
	}
}

func newScenariosShowCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Print the raw YAML of a built-in scenario by name",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScenariosShow(cmd.OutOrStdout(), args[0])
		},
	}
}

func newScenariosArchetypesCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "archetypes <name>",
		Short: "Print the tenant archetype configs of a built-in scenario",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScenariosArchetypes(cmd.OutOrStdout(), args[0])
		},
	}
}

// --- runners ------------------------------------------------------------

func runScenariosList(out io.Writer) error {
	names := scenarios.Names()
	if len(names) == 0 {
		_, err := fmt.Fprintln(out, "no built-in scenarios bundled in this build")
		return err
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tTARGET TPS\tDURATION\tTENANTS\tARCHETYPES\tDESCRIPTION"); err != nil {
		return err
	}
	for _, name := range names {
		data, err := scenarios.Read(name)
		if err != nil {
			return fmt.Errorf("read built-in %q: %w", name, err)
		}
		doc, err := scenario.LoadFromBytes(data)
		if err != nil {
			return fmt.Errorf("parse built-in %q: %w", name, err)
		}
		s := doc.Scenario
		if _, err := fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%d\t%s\n",
			s.Name, s.TargetTPS, s.Duration.Std(), s.Tenants.Count,
			len(s.Tenants.Archetypes), truncateLine(s.Description, 60)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func runScenariosValidate(out, errOut io.Writer, path string) error {
	doc, err := scenario.LoadFromFile(path)
	if err != nil {
		// Load errors (file missing, YAML parse error, unknown fields) come
		// back fully formed from LoadFromFile. Return them and let main print
		// once — printing here too would double the message because root.go
		// keeps SilenceErrors=true off the leaf.
		return err
	}
	if errs := scenario.Validate(doc); errs.HasErrors() {
		// Validation errors are listed individually on stderr; the returned
		// error is the summary line, which main also prints to stderr.
		// Together they read as: "<errs...>\n<file>: N validation error(s)".
		for _, e := range errs {
			fmt.Fprintln(errOut, e.Error())
		}
		return fmt.Errorf("%s: %d validation error(s)", path, len(errs))
	}
	_, err = fmt.Fprintf(out, "%s: ok (schema_version=%d, name=%s, archetypes=%d)\n",
		path, doc.Scenario.SchemaVersion, doc.Scenario.Name, len(doc.Scenario.Tenants.Archetypes))
	return err
}

func runScenariosShow(out io.Writer, name string) error {
	data, err := scenarios.Read(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no built-in scenario named %q (try `aforo-loadgen scenarios list`)", name)
		}
		return err
	}
	_, err = out.Write(data)
	return err
}

func runScenariosArchetypes(out io.Writer, name string) error {
	data, err := scenarios.Read(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no built-in scenario named %q (try `aforo-loadgen scenarios list`)", name)
		}
		return err
	}
	doc, err := scenario.LoadFromBytes(data)
	if err != nil {
		return fmt.Errorf("parse %q: %w", name, err)
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tWEIGHT\tPRICING\tBILLING\tPRODUCT TYPES\tCUSTOMERS"); err != nil {
		return err
	}
	for _, a := range doc.Scenario.Tenants.Archetypes {
		products := make([]string, len(a.ProductTypes))
		for i, p := range a.ProductTypes {
			products[i] = string(p)
		}
		if _, err := fmt.Fprintf(tw, "%s\t%.4f\t%s\t%s\t%s\t%d\n",
			a.Name, a.Weight, a.PricingModel, a.BillingMode,
			strings.Join(products, ","), a.CustomerCount); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// truncateLine snips a description to maxLen bytes, appending "…" when cut.
// Used in the list view; not safe for arbitrary multibyte UTF-8 (no scenario
// description in this repo contains multibyte characters).
func truncateLine(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
}
