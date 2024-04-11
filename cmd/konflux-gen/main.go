package main

import (
	"bytes"
	"crypto/md5"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	gyaml "github.com/ghodss/yaml"
	"github.com/spf13/pflag"

	cioperatorapi "github.com/openshift/ci-tools/pkg/api"
)

const (
	openShiftReleasePathFlag = "openshift-release-path"
	applicationNameFlag      = "application-name"
	includesFlag             = "includes"
	excludesFlag             = "excludes"
	excludeImagesFlag        = "exclude-images"
	outputFlag               = "output"
)

//go:embed application.template.yaml
var ApplicationTemplate embed.FS

//go:embed dockerfile-component.template.yaml
var DockerfileComponentTemplate embed.FS

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {

	openshiftReleasePath := ""
	applicationName := ""
	var rawIncludes []string
	var rawExcludes []string
	var rawExcludeImages []string
	output := ""

	pflag.StringVar(&openshiftReleasePath, openShiftReleasePathFlag, "", "openshift/release repository path")
	pflag.StringVar(&applicationName, applicationNameFlag, "", "Konflux application name")
	pflag.StringVar(&output, outputFlag, "", "output path")
	pflag.StringArrayVar(&rawIncludes, includesFlag, nil, "Regex to select CI config files to include")
	pflag.StringArrayVar(&rawExcludes, excludesFlag, nil, "Regex to select CI config files to exclude")
	pflag.StringArrayVar(&rawExcludeImages, excludeImagesFlag, nil, "Regex to select CI config images to exclude")
	pflag.Parse()

	if openshiftReleasePath == "" {
		return fmt.Errorf("expected %q flag to be non empty", openShiftReleasePathFlag)
	}
	if len(rawIncludes) == 0 {
		return fmt.Errorf("expected %q flag to be non empty", includesFlag)
	}

	includes, err := toRegexp(rawIncludes)
	if err != nil {
		return fmt.Errorf("failed to create %q regular expressions: %w", includesFlag, err)
	}
	excludes, err := toRegexp(rawExcludes)
	if err != nil {
		return fmt.Errorf("failed to create %q regular expressions: %w", excludesFlag, err)
	}
	excludeImages, err := toRegexp(rawExcludeImages)
	if err != nil {
		return fmt.Errorf("failed to create %q regular expressions: %w", excludeImagesFlag, err)
	}

	configs, err := collectConfigurations(openshiftReleasePath, includes, excludes)
	if err != nil {
		return err
	}

	log.Printf("Found %d configs", len(configs))

	funcs := template.FuncMap{
		"sanitize": sanitize,
		"truncate": truncate,
	}

	applicationTemplate, err := template.New("application.template.yaml").Funcs(funcs).ParseFS(ApplicationTemplate, "*.yaml")
	if err != nil {
		return fmt.Errorf("failed to parse application template: %w", err)
	}
	dockerfileComponentTemplate, err := template.New("dockerfile-component.template.yaml").Funcs(funcs).ParseFS(DockerfileComponentTemplate, "*.yaml")
	if err != nil {
		return fmt.Errorf("failed to parse dockerfile component template: %w", err)
	}

	applications := make(map[string]map[string]DockerfileApplicationConfig, 8)
	for _, c := range configs {
		appKey := truncate(sanitize(applicationName))
		if _, ok := applications[appKey]; !ok {
			applications[appKey] = make(map[string]DockerfileApplicationConfig, 8)
		}
		for _, ib := range c.Images {

			ignore := false
			for _, r := range excludeImages {
				if r.MatchString(string(ib.To)) {
					ignore = true
					break
				}
			}
			if ignore {
				continue
			}

			applications[appKey][dockerfileComponentKey(c.ReleaseBuildConfiguration, ib)] = DockerfileApplicationConfig{
				ApplicationName:           applicationName,
				ReleaseBuildConfiguration: c.ReleaseBuildConfiguration,
				Path:                      c.Path,
				ProjectDirectoryImageBuildStepConfiguration: ib,
			}
		}
	}

	for appKey, components := range applications {

		for componentKey, config := range components {
			buf := &bytes.Buffer{}

			appPath := filepath.Join(output, "applications", appKey, fmt.Sprintf("%s.yaml", appKey))
			if err := os.MkdirAll(filepath.Dir(appPath), 0777); err != nil {
				return fmt.Errorf("failed to create directory for %q: %w", appPath, err)
			}

			if err := applicationTemplate.Execute(buf, config); err != nil {
				return fmt.Errorf("failed to execute template for application %q: %w", appKey, err)
			}
			if err := os.WriteFile(appPath, buf.Bytes(), 0777); err != nil {
				return fmt.Errorf("failed to write application file %q: %w", appPath, err)
			}

			buf.Reset()

			componentPath := filepath.Join(output, "applications", appKey, "components", fmt.Sprintf("%s.yaml", componentKey))
			if err := os.MkdirAll(filepath.Dir(componentPath), 0777); err != nil {
				return fmt.Errorf("failed to create directory for %q: %w", componentPath, err)
			}

			if err := dockerfileComponentTemplate.Execute(buf, config); err != nil {
				return fmt.Errorf("failed to execute template for component %q: %w", componentKey, err)
			}
			if err := os.WriteFile(componentPath, buf.Bytes(), 0777); err != nil {
				return fmt.Errorf("failed to write component file %q: %w", componentPath, err)
			}
		}
	}

	return nil
}

func collectConfigurations(openshiftReleasePath string, includes []*regexp.Regexp, excludes []*regexp.Regexp) ([]Config, error) {
	configs := make([]Config, 0, 8)
	err := filepath.WalkDir(openshiftReleasePath, func(path string, info fs.DirEntry, err error) error {
		if info.IsDir() {
			return nil
		}

		matchablePath, err := filepath.Rel(openshiftReleasePath, path)
		if err != nil {
			return fmt.Errorf("failed to get relative path for %q (base path %q): %w", matchablePath, openshiftReleasePath, err)
		}

		shouldInclude := false
		for _, i := range includes {
			if i.MatchString(matchablePath) {
				shouldInclude = true
			}
			for _, x := range excludes {
				if x.MatchString(matchablePath) {
					shouldInclude = false
				}
			}
		}
		if !shouldInclude {
			return nil
		}

		log.Printf("Parsing file %q\n", path)

		config, err := parseConfig(path)
		if err != nil {
			return fmt.Errorf("failed to parse CI config in %q: %w", path, err)
		}

		configs = append(configs, Config{
			ReleaseBuildConfiguration: *config,
			Path:                      path,
		})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed while walking directory %q: %w\n", openshiftReleasePath, err)
	}
	return configs, nil
}

type Config struct {
	cioperatorapi.ReleaseBuildConfiguration
	Path string
}

type DockerfileApplicationConfig struct {
	ApplicationName                             string
	ReleaseBuildConfiguration                   cioperatorapi.ReleaseBuildConfiguration
	Path                                        string
	ProjectDirectoryImageBuildStepConfiguration cioperatorapi.ProjectDirectoryImageBuildStepConfiguration
}

func parseConfig(path string) (*cioperatorapi.ReleaseBuildConfiguration, error) {
	// Going directly from YAML raw input produces unexpected configs (due to missing YAML tags),
	// so we convert YAML to JSON and unmarshal the struct from the JSON object.
	y, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file %q: %w", path, err)
	}
	j, err := gyaml.YAMLToJSON(y)
	if err != nil {
		return nil, fmt.Errorf("failed to convert YAML to JSON: %w", err)
	}

	jobConfig := &cioperatorapi.ReleaseBuildConfiguration{}
	if err := json.Unmarshal(j, jobConfig); err != nil {
		return nil, fmt.Errorf("failed to unmarshal file %q into %T: %w", path, jobConfig, err)
	}

	return jobConfig, err
}

func toRegexp(rawRegexps []string) ([]*regexp.Regexp, error) {
	regexps := make([]*regexp.Regexp, 0, len(rawRegexps))
	for _, i := range rawRegexps {
		r, err := regexp.Compile(i)
		if err != nil {
			return regexps, fmt.Errorf("regex %q doesn't compile: %w", i, err)
		}
		regexps = append(regexps, r)
	}
	return regexps, nil
}

func dockerfileComponentKey(cfg cioperatorapi.ReleaseBuildConfiguration, ib cioperatorapi.ProjectDirectoryImageBuildStepConfiguration) string {
	return fmt.Sprintf("%s-%s-%s-%s", cfg.Metadata.Org, cfg.Metadata.Repo, cfg.Metadata.Branch, ib.To)
}

func applicationKey(cfg cioperatorapi.ReleaseBuildConfiguration) string {
	return fmt.Sprintf("%s-%s-%s", cfg.Metadata.Org, cfg.Metadata.Repo, cfg.Metadata.Branch)
}

func sanitize(input interface{}) string {
	in := fmt.Sprintf("%s", input)
	// TODO very basic name sanitizer
	return strings.ReplaceAll(strings.ReplaceAll(in, ".", ""), " ", "-")
}

func truncate(input interface{}) string {
	in := fmt.Sprintf("%s", input)
	// TODO very basic name sanitizer
	return Name(in, "")
}

const (
	longest = 63
	md5Len  = 32
	head    = longest - md5Len // How much to truncate to fit the hash.
)

// Name generates a name for the resource based upon the parent resource and suffix.
// If the concatenated name is longer than K8s permits the name is hashed and truncated to permit
// construction of the resource, but still keeps it unique.
// If the suffix itself is longer than 31 characters, then the whole string will be hashed
// and `parent|hash|suffix` will be returned, where parent and suffix will be trimmed to
// fit (prefix of parent at most of length 31, and prefix of suffix at most length 30).
func Name(parent, suffix string) string {
	n := parent
	if len(parent) > (longest - len(suffix)) {
		// If the suffix is longer than the longest allowed suffix, then
		// we hash the whole combined string and use that as the suffix.
		if head-len(suffix) <= 0 {
			//nolint:gosec // No strong cryptography needed.
			h := md5.Sum([]byte(parent + suffix))
			// 1. trim parent, if needed
			if head < len(parent) {
				parent = parent[:head]
			}
			// Format the return string, if it's shorter than longest: pad with
			// beginning of the suffix. This happens, for example, when parent is
			// short, but the suffix is very long.
			ret := parent + fmt.Sprintf("%x", h)
			if d := longest - len(ret); d > 0 {
				ret += suffix[:d]
			}
			return makeValidName(ret)
		}
		//nolint:gosec // No strong cryptography needed.
		n = fmt.Sprintf("%s%x", parent[:head-len(suffix)], md5.Sum([]byte(parent)))
	}
	return n + suffix
}

var isAlphanumeric = regexp.MustCompile(`^[a-zA-Z0-9]*$`)

// If due to trimming above we're terminating the string with a non-alphanumeric
// character, remove it.
func makeValidName(n string) string {
	for i := len(n) - 1; !isAlphanumeric.MatchString(string(n[i])); i-- {
		n = n[:len(n)-1]
	}
	return n
}
