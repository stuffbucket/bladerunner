package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Cobra renders --instance in the help of every command, because it is a
// persistent flag on the root. Most commands then ignored it (issue #9). A flag
// that a command shows and silently drops is worse than one it rejects: the
// user reads the help, uses the flag, and gets an answer about a different VM.
//
// So each command declares what it does with --instance, and an UNDECLARED
// command refuses it. A new verb therefore cannot inherit the defect: until
// somebody decides, `br newverb --instance foo` says so instead of quietly
// acting on the default.

const (
	// annotationInstance is the command annotation that declares the policy. A
	// command with no annotation inherits its parent's, and the root's absence
	// of one means "refused".
	annotationInstance = "bladerunner.instance"
	// annotationInstanceHint is appended to a refusal, naming the verb or flag
	// to reach for instead. Optional.
	annotationInstanceHint = "bladerunner.instance-hint"

	// instanceHonored marks a command that resolves --instance through
	// resolveInstanceTarget and acts on what comes back.
	instanceHonored = "honored"
	// instanceAllInstances marks a command that deliberately spans every
	// instance, so selecting one would mean nothing.
	instanceAllInstances = "all"
	// instanceRefused marks a command that does not act on a selected instance.
	// It is also what an undeclared command resolves to.
	instanceRefused = "refused"
)

// instancePolicy reports how cmd treats --instance, inheriting the nearest
// ancestor's declaration so a subcommand tree ('br web approve', 'br user add')
// is declared once at its root and can still override.
func instancePolicy(cmd *cobra.Command) string {
	for c := cmd; c != nil; c = c.Parent() {
		if policy := c.Annotations[annotationInstance]; policy != "" {
			return policy
		}
	}
	return instanceRefused
}

// instanceHint returns the "reach for this instead" sentence for cmd, using the
// same nearest-ancestor rule as instancePolicy.
func instanceHint(cmd *cobra.Command) string {
	for c := cmd; c != nil; c = c.Parent() {
		if hint := c.Annotations[annotationInstanceHint]; hint != "" {
			return hint
		}
	}
	return ""
}

// checkInstanceFlag is the root command's PersistentPreRunE: it refuses
// --instance on a command that does not act on a selected instance.
//
// It reads instanceFlag rather than selectedInstanceName, so only the FLAG
// triggers the refusal. A BLADERUNNER_INSTANCE left in the environment is a
// standing preference, not a claim about this invocation, and must never make
// `br disks` fail.
func checkInstanceFlag(cmd *cobra.Command, _ []string) error {
	if instanceFlag == "" {
		return nil
	}
	switch instancePolicy(cmd) {
	case instanceHonored, instanceAllInstances:
		return nil
	}
	err := fmt.Errorf("--instance selects a VM to act on; '%s' does not act on one", cmd.CommandPath())
	if hint := instanceHint(cmd); hint != "" {
		err = fmt.Errorf("%w\n  %s", err, hint)
	}
	return jsonOrError(err)
}

// declareInstancePolicy stamps policy onto each command. Kept as one table in
// root.go's init rather than on each command literal, so the whole policy is
// reviewable in one place.
func declareInstancePolicy(policy string, cmds ...*cobra.Command) {
	for _, c := range cmds {
		if c.Annotations == nil {
			c.Annotations = map[string]string{}
		}
		c.Annotations[annotationInstance] = policy
	}
}

// declareInstanceHint records what to reach for instead, for a command whose
// refusal deserves more than "not here".
func declareInstanceHint(cmd *cobra.Command, hint string) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[annotationInstanceHint] = hint
}
