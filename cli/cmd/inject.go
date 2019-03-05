package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	jsonpatch "github.com/evanphx/json-patch"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/linkerd/linkerd2/pkg/inject"
	"github.com/linkerd/linkerd2/pkg/k8s"
)

const (
	// for inject reports
	hostNetworkDesc    = "pods do not use host networking"
	sidecarDesc        = "pods do not have a 3rd party proxy or initContainer already injected"
	injectDisabledDesc = "pods are not annotated to disable injection"
	unsupportedDesc    = "at least one resource injected"
	udpDesc            = "pod specs do not include UDP ports"
)

type injectOptions struct {
	proxyConfigOptions
}

type resourceTransformerInject struct {
	configs
	proxyOutboundCapacity map[string]uint
}

func runInjectCmd(inputs []io.Reader, errWriter, outWriter io.Writer, conf configs) int {
	return transformInput(inputs, errWriter, outWriter, resourceTransformerInject{
		configs: conf,
	})
}

func newInjectOptions() *injectOptions {
	return &injectOptions{}
}

func newCmdInject() *cobra.Command {
	options := newInjectOptions()

	cmd := &cobra.Command{
		Use:   "inject [flags] CONFIG-FILE",
		Short: "Add the Linkerd proxy to a Kubernetes config",
		Long: `Add the Linkerd proxy to a Kubernetes config.

You can inject resources contained in a single file, inside a folder and its
sub-folders, or coming from stdin.`,
		Example: `  # Inject all the deployments in the default namespace.
  kubectl get deploy -o yaml | linkerd inject - | kubectl apply -f -

  # Download a resource and inject it through stdin.
  curl http://url.to/yml | linkerd inject - | kubectl apply -f -

  # Inject all the resources inside a folder and its sub-folders.
  linkerd inject <folder> | kubectl apply -f -`,
		RunE: func(cmd *cobra.Command, args []string) error {

			if len(args) < 1 {
				return fmt.Errorf("please specify a kubernetes resource file")
			}

			if err := options.validate(); err != nil {
				return err
			}

			in, err := read(args[0])
			if err != nil {
				return err
			}

			conf, err := fetchConfigsFromK8s()
			if err != nil {
				return err
			}
			conf.overrideFromOptions(options)

			exitCode := uninjectAndInject(in, stderr, stdout, conf)
			os.Exit(exitCode)
			return nil
		},
	}

	addProxyConfigFlags(cmd, &options.proxyConfigOptions)

	return cmd
}

func uninjectAndInject(inputs []io.Reader, errWriter, outWriter io.Writer, conf configs) int {
	var out bytes.Buffer
	if exitCode := runUninjectSilentCmd(inputs, errWriter, &out, conf); exitCode != 0 {
		return exitCode
	}
	return runInjectCmd([]io.Reader{&out}, errWriter, outWriter, conf)
}

func (rt resourceTransformerInject) transform(bytes []byte) ([]byte, []inject.Report, error) {
	conf := inject.NewResourceConfig(rt.global, rt.proxy)
	if len(rt.proxyOutboundCapacity) > 0 {
		conf = conf.WithProxyOutboundCapacity(rt.proxyOutboundCapacity)
	}
	nonEmpty, err := conf.ParseMeta(bytes)
	if err != nil {
		return nil, nil, err
	}
	if !nonEmpty {
		r := inject.Report{UnsupportedResource: true}
		return bytes, []inject.Report{r}, nil
	}
	p, reports, err := conf.GetPatch(bytes, inject.ShouldInjectCLI)
	if err != nil {
		return nil, nil, err
	}
	if p.IsEmpty() {
		return bytes, reports, nil
	}
	p.AddCreatedByPodAnnotation(k8s.CreatedByAnnotationValue())
	patchJSON, err := p.Marshal()
	if patchJSON == nil {
		return bytes, reports, nil
	}
	if err != nil {
		return nil, nil, err
	}
	log.Debugf("patch generated: %s", patchJSON)
	patch, err := jsonpatch.DecodePatch(patchJSON)
	if err != nil {
		return nil, nil, err
	}
	origJSON, err := yaml.YAMLToJSON(bytes)
	if err != nil {
		return nil, nil, err
	}
	injectedJSON, err := patch.Apply(origJSON)
	if err != nil {
		return nil, nil, err
	}
	injectedYAML, err := conf.JSONToYAML(injectedJSON)
	if err != nil {
		return nil, nil, err
	}
	return injectedYAML, reports, nil
}

func (resourceTransformerInject) generateReport(reports []inject.Report, output io.Writer) {
	injected := []inject.Report{}
	hostNetwork := []string{}
	sidecar := []string{}
	udp := []string{}
	injectDisabled := []string{}
	warningsPrinted := verbose

	for _, r := range reports {
		if r.Injectable() {
			injected = append(injected, r)
		}

		if r.HostNetwork {
			hostNetwork = append(hostNetwork, r.ResName())
			warningsPrinted = true
		}

		if r.Sidecar {
			sidecar = append(sidecar, r.ResName())
			warningsPrinted = true
		}

		if r.UDP {
			udp = append(udp, r.ResName())
			warningsPrinted = true
		}

		if r.InjectDisabled {
			injectDisabled = append(injectDisabled, r.ResName())
			warningsPrinted = true
		}
	}

	//
	// Warnings
	//

	// Leading newline to separate from yaml output on stdout
	output.Write([]byte("\n"))

	if len(hostNetwork) > 0 {
		output.Write([]byte(fmt.Sprintf("%s \"hostNetwork: true\" detected in %s\n", warnStatus, strings.Join(hostNetwork, ", "))))
	} else if verbose {
		output.Write([]byte(fmt.Sprintf("%s %s\n", okStatus, hostNetworkDesc)))
	}

	if len(sidecar) > 0 {
		output.Write([]byte(fmt.Sprintf("%s known 3rd party sidecar detected in %s\n", warnStatus, strings.Join(sidecar, ", "))))
	} else if verbose {
		output.Write([]byte(fmt.Sprintf("%s %s\n", okStatus, sidecarDesc)))
	}

	if len(injectDisabled) > 0 {
		output.Write([]byte(fmt.Sprintf("%s \"%s: %s\" annotation set on %s\n",
			warnStatus, k8s.ProxyInjectAnnotation, k8s.ProxyInjectDisabled, strings.Join(injectDisabled, ", "))))
	} else if verbose {
		output.Write([]byte(fmt.Sprintf("%s %s\n", okStatus, injectDisabledDesc)))
	}

	if len(injected) == 0 {
		output.Write([]byte(fmt.Sprintf("%s no supported objects found\n", warnStatus)))
		warningsPrinted = true
	} else if verbose {
		output.Write([]byte(fmt.Sprintf("%s %s\n", okStatus, unsupportedDesc)))
	}

	if len(udp) > 0 {
		verb := "uses"
		if len(udp) > 1 {
			verb = "use"
		}
		output.Write([]byte(fmt.Sprintf("%s %s %s \"protocol: UDP\"\n", warnStatus, strings.Join(udp, ", "), verb)))
	} else if verbose {
		output.Write([]byte(fmt.Sprintf("%s %s\n", okStatus, udpDesc)))
	}

	//
	// Summary
	//
	if warningsPrinted {
		output.Write([]byte("\n"))
	}

	for _, r := range reports {
		if r.Injectable() {
			output.Write([]byte(fmt.Sprintf("%s \"%s\" injected\n", r.Kind, r.Name)))
		} else {
			if r.Kind != "" {
				output.Write([]byte(fmt.Sprintf("%s \"%s\" skipped\n", r.Kind, r.Name)))
			} else {
				output.Write([]byte(fmt.Sprintf("document missing \"kind\" field, skipped\n")))
			}
		}
	}

	// Trailing newline to separate from kubectl output if piping
	output.Write([]byte("\n"))
}
