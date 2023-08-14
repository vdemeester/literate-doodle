package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"dagger.io/dagger"
	v1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	"github.com/tektoncd/pipeline/pkg/client/clientset/versioned/scheme"
	"github.com/tektoncd/pipeline/pkg/names"
	"github.com/tektoncd/pipeline/pkg/reconciler/taskrun/resources"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	defaultScriptPreamble = "#!/bin/sh\nset -e\n"
	scriptsDir            = "/tekton/scripts"
)

func main() {
	if err := build(context.Background()); err != nil {
		log.Panic(err)
	}
}

func build(ctx context.Context) error {
	b, err := readTask("golang-build.yaml")
	if err != nil {
		return err
	}
	b.SetDefaults(ctx)

	tr := &v1.TaskRun{
		Spec: v1.TaskRunSpec{
			Params: []v1.Param{{
				Name:  "packages",
				Value: v1.ParamValue{StringVal: "."},
			}},
		},
	}
	spec, err := applySubstitution(ctx, tr, &b.Spec, b.Name)
	if err != nil {
		return err
	}

	client, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
	if err != nil {
		return err
	}
	defer client.Close()

	project := client.Git("https://github.com/tektoncd/pipeline").Branch("main").Tree()

	client = client.Pipeline("golang-build")
	outputs := client.Directory()

	workspaces := []myWorkspaces{}
	for _, workspace := range spec.Workspaces {
		workspaces = append(workspaces, myWorkspaces{name: workspace.Name, daggerDirectory: client.Directory(), mountPath: workspace.MountPath})
	}

	for _, step := range spec.Steps {
		tc := client.Container().
			From(step.Image).
			WithWorkdir("/workspace/source") // FIXME: support variable interpolation and stuff
		if step.Script != "" {
			script := step.Script
			filename := names.SimpleNameGenerator.RestrictLengthWithRandomSuffix(step.Name)
			cleaned := strings.TrimSpace(script)
			hasShebang := strings.HasPrefix(cleaned, "#!")
			if !hasShebang {
				script = defaultScriptPreamble + script
			}
			scriptFile := filepath.Join(scriptsDir, filename)
			tc = tc.WithNewFile(scriptFile, dagger.ContainerWithNewFileOpts{Contents: script, Permissions: 0755}).
				WithExec([]string{scriptFile})
		} else {
			commands := append(step.Command, step.Args...)
			tc = tc.WithExec(commands)
		}
		// TODO: Support : step.Workspaces
		for _, workspace := range workspaces {
			// FIXME support dynamic plugin....
			fmt.Println("workspace", workspace)
			if workspace.name == "source" {
				tc = tc.WithDirectory("/workspace/source", project)
			} else {
				mountPath := workspace.mountPath
				if mountPath == "" {
					mountPath = "/workspace/" + workspace.name
				}
				tc = tc.WithDirectory(mountPath, workspace.daggerDirectory)
			}
		}
		outputs = outputs.WithDirectory("/workspace/source", tc.Directory("/workspace/source"))
	}
	ok, err := outputs.Export(ctx, "foo")
	if err != nil {
		return err
	}

	if !ok {
		return fmt.Errorf("Not OK")
	}
	return nil
}

func readTask(path string) (v1.Task, error) {
	var b v1.Task
	data, err := os.ReadFile("golang-build.yaml")
	if err != nil {
		return b, err
	}
	if err := readKubernetesYAML(data, &b); err != nil {
		return b, err
	}
	return b, nil
}

func readKubernetesYAML(data []byte, i runtime.Object) error {
	if _, _, err := scheme.Codecs.UniversalDeserializer().Decode(data, nil, i); err != nil {
		return fmt.Errorf("mustParseYAML (%s): %w", string(data), err)
	}
	return nil
}

func applySubstitution(ctx context.Context, tr *v1.TaskRun, ts *v1.TaskSpec, taskName string) (v1.TaskSpec, error) {
	var defaults []v1.ParamSpec
	if len(ts.Params) > 0 {
		defaults = append(defaults, ts.Params...)
	}
	// Apply parameter substitution from the taskrun.
	ts = resources.ApplyParameters(ctx, ts, tr, defaults...)

	// Apply context substitution from the taskrun
	ts = resources.ApplyContexts(ts, taskName, tr)

	// TODO(vdemeester) support PipelineResource ?
	// Apply bound resource substitution from the taskrun.
	// ts = resources.ApplyResources(ts, inputResources, "inputs")
	// ts = resources.ApplyResources(ts, outputResources, "outputs")

	// Apply workspace resource substitution
	workspaceVolumes := map[string]corev1.Volume{}
	for _, v := range tr.Spec.Workspaces {
		workspaceVolumes[v.Name] = corev1.Volume{Name: v.Name}
	}
	ts = resources.ApplyWorkspaces(ctx, ts, ts.Workspaces, tr.Spec.Workspaces, workspaceVolumes)

	// Apply task result substitution
	ts = resources.ApplyTaskResults(ts)

	// Apply step exitCode path substitution
	ts = resources.ApplyStepExitCodePath(ts)

	if err := ts.Validate(ctx); err != nil {
		return *ts, err
	}
	return *ts, nil
}

type myWorkspaces struct {
	daggerDirectory *dagger.Directory
	name            string
	mountPath       string
}
