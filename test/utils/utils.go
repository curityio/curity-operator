package utils

import (
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	"github.com/onsi/gomega"
)

// Run executes the provided command within the project directory.
func Run(name string, args ...string) (string, error) {
	dir, _ := GetProjectDir()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %s\n", err)
		return "", fmt.Errorf("chdir dir: %s", err.Error())
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %s\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s failed with error: (%v) %s", command, err.Error(), string(output))
	}

	return strings.TrimSpace(string(output)), nil
}

// RunShell executes the provided arguments as a shell command.
func RunShell(args ...string) (string, error) {
	return Run("sh", "-c", strings.Join(args, " "))
}

// GetNonEmptyLines splits output by newlines and returns non-empty lines.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}
	return res
}

// GetProjectDir returns the project root directory.
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, err
	}
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}

// AssertError fails the test if err is not nil.
func AssertError(err error, msg ...string) {
	if err != nil {
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), fmt.Sprintf("[%s] %s", err.Error(), strings.Join(msg, " ")))
	}
}

// GetKind returns the type name of the given object.
func GetKind(obj interface{}) string {
	t := reflect.TypeOf(obj)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t.Name()
}
