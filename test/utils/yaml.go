package utils

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"text/template"

	"sigs.k8s.io/yaml"
)

// ApplyFixtureTemplate renders a Go template fixture and applies it to the cluster.
func ApplyFixtureTemplate(templatePath string, namespace string, values map[string]interface{}) string {
	templateData, err := os.ReadFile(templatePath)
	AssertError(err)

	tmpl, err := template.New("resource").Parse(string(templateData))
	AssertError(err)

	var buffer bytes.Buffer
	err = tmpl.Execute(&buffer, values)
	AssertError(err)

	args := []string{"apply", "-f-"}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}

	cmd := exec.Command("kubectl", args...)
	cmd.Stdin = &buffer
	output, err := cmd.CombinedOutput()
	AssertError(err, string(output))

	return strings.TrimSpace(string(output))
}

// ApplyRawYAML applies a raw YAML string to the cluster via kubectl.
func ApplyRawYAML(yamlContent string, namespace string) string {
	args := []string{"apply", "-f-"}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}

	cmd := exec.Command("kubectl", args...)
	cmd.Stdin = strings.NewReader(yamlContent)
	output, err := cmd.CombinedOutput()
	AssertError(err, string(output))

	return strings.TrimSpace(string(output))
}

// GetResourceFromFile reads a YAML file and unmarshals it into the provided object.
func GetResourceFromFile(filePath string, obj interface{}) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, obj)
}
