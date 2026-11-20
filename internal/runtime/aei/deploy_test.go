package aei

import (
	"net/url"
	"os"
	"path"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jackfrancis/agent-execution-interface/sdks/go/aeiapp"
)

// dispatchAudience is the audience the aei-controller's dispatch TokenReview requires
// (its -dispatch-audience default). A token projected for any other audience cannot
// dispatch.
const dispatchAudience = "aei-dispatch"

// dispatchPort is the aei-controller Service's dispatch port, apart from the runtime
// ABI and metrics on 8080, so a NetworkPolicy can keep runtime pods off dispatch.
const dispatchPort = "8081"

type configMap struct {
	Data map[string]string `yaml:"data"`
}

type statefulSet struct {
	Spec struct {
		Template struct {
			Spec struct {
				AutomountServiceAccountToken *bool `yaml:"automountServiceAccountToken"`
				Containers                   []struct {
					Name         string `yaml:"name"`
					VolumeMounts []struct {
						Name      string `yaml:"name"`
						MountPath string `yaml:"mountPath"`
						ReadOnly  bool   `yaml:"readOnly"`
					} `yaml:"volumeMounts"`
				} `yaml:"containers"`
				Volumes []struct {
					Name      string `yaml:"name"`
					Projected *struct {
						Sources []struct {
							ServiceAccountToken *struct {
								Path              string `yaml:"path"`
								Audience          string `yaml:"audience"`
								ExpirationSeconds int64  `yaml:"expirationSeconds"`
							} `yaml:"serviceAccountToken"`
						} `yaml:"sources"`
					} `yaml:"projected"`
				} `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func loadManifest(t *testing.T, file string, into any) {
	t.Helper()
	b, err := os.ReadFile(path.Join("../../../deploy/k8s/base", file))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, into); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
}

// The web pod's only cluster credential is a ServiceAccount token projected for AEI's
// dispatch audience, the backend reads it from the path gofer-config names, and it
// dispatches to the controller's dispatch listener. Each half lives in a different
// manifest, so this holds them together.
func TestDeploymentDispatchesWithTheProjectedDispatchToken(t *testing.T) {
	var cm configMap
	loadManifest(t, "configmap.yaml", &cm)
	var sts statefulSet
	loadManifest(t, "statefulset.yaml", &sts)
	pod := sts.Spec.Template.Spec

	if a := pod.AutomountServiceAccountToken; a == nil || *a {
		t.Error("the web pod must not mount the default ServiceAccount token (automountServiceAccountToken: false)")
	}

	// The projected dispatch token: the one serviceAccountToken source in the pod.
	var volume, tokenFile string
	for _, v := range pod.Volumes {
		if v.Projected == nil {
			continue
		}
		for _, src := range v.Projected.Sources {
			sat := src.ServiceAccountToken
			if sat == nil {
				continue
			}
			if volume != "" {
				t.Fatalf("more than one projected ServiceAccount token (%s, %s)", volume, v.Name)
			}
			volume, tokenFile = v.Name, sat.Path
			if sat.Audience != dispatchAudience {
				t.Errorf("projected token audience = %q, want %q", sat.Audience, dispatchAudience)
			}
			// The kubelet refuses less than 10 minutes; AEI's guidance is an hour.
			if sat.ExpirationSeconds < 600 || sat.ExpirationSeconds > 3600 {
				t.Errorf("projected token expirationSeconds = %d, want 600..3600", sat.ExpirationSeconds)
			}
		}
	}
	if volume == "" {
		t.Fatal("no projected ServiceAccount token volume")
	}

	var mountPath string
	for _, c := range pod.Containers {
		if c.Name != "server" {
			continue
		}
		for _, m := range c.VolumeMounts {
			if m.Name == volume {
				mountPath = m.MountPath
				if !m.ReadOnly {
					t.Errorf("%s is mounted writable", volume)
				}
			}
		}
	}
	if mountPath == "" {
		t.Fatalf("the server container does not mount %s", volume)
	}

	if got, want := cm.Data[aeiapp.TokenPathEnv], path.Join(mountPath, tokenFile); got != want {
		t.Errorf("%s = %q, want the projected token at %q", aeiapp.TokenPathEnv, got, want)
	}

	endpoint, err := url.Parse(cm.Data["AEI_DISPATCH_ENDPOINT"])
	if err != nil {
		t.Fatalf("AEI_DISPATCH_ENDPOINT: %v", err)
	}
	if endpoint.Port() != dispatchPort {
		t.Errorf("AEI_DISPATCH_ENDPOINT = %q, want the dispatch listener on port %s", endpoint, dispatchPort)
	}
}
