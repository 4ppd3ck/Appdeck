//go:build darwin

// macOS code-signature handling for imported apps. A Mach-O lifted out of
// its .app bundle (or re-packed through formats that drop metadata) breaks
// the seal its developer signed — the kernel then kills it at launch with
// nothing but an empty log. Import verifies what landed and repairs what it
// can: see importSignatureState. Every invocation is logged into the import
// job (exact command line, trimmed output) so the dashboard log shows what
// ran.
package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// logCmd echoes the command line into the import log the way a shell would.
func logCmd(job *jobLog, cmd *exec.Cmd) {
	job.logf("$ %s", strings.Join(cmd.Args, " "))
}

// verifyCodeSignature checks a binary or .app bundle in place. nil means the
// seal is intact and macOS will launch it as the vendor signed it.
func verifyCodeSignature(path string, job *jobLog) error {
	cmd := exec.Command("/usr/bin/codesign", "--verify", "--strict", path)
	logCmd(job, cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v (%s)", err, lastLine(out))
	}
	return nil
}

// adHocResign re-signs a binary or bundle with a local ad-hoc identity (no
// certificate, no notarization): the app launches and runs, but macOS no
// longer attributes it to the developer. deep signs a bundle's nested code
// (frameworks, helpers) before the outer seal — the shape a lifted bundle
// needs, since its nested seals broke with the outer one.
func adHocResign(path string, deep bool, job *jobLog) error {
	args := []string{"--force", "--sign", "-"}
	if deep {
		args = append(args, "--deep")
	}
	args = append(args, path)
	cmd := exec.Command("/usr/bin/codesign", args...)
	logCmd(job, cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v (%s)", err, lastLine(out))
	}
	return nil
}

// lastLine is codesign's habit of echoing the invoked command before its
// diagnostics — errors want the diagnostic, not the echo.
func lastLine(out []byte) string {
	reason := strings.TrimSpace(string(out))
	if i := strings.LastIndex(reason, "\n"); i >= 0 {
		reason = reason[i+1:]
	}
	return reason
}
