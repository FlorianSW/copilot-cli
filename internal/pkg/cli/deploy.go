// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"slices"

	"github.com/aws/copilot-cli/cmd/copilot/template"
	"github.com/aws/copilot-cli/internal/pkg/aws/identity"
	"github.com/aws/copilot-cli/internal/pkg/aws/sessions"
	"github.com/aws/copilot-cli/internal/pkg/cli/group"
	"github.com/aws/copilot-cli/internal/pkg/config"
	"github.com/aws/copilot-cli/internal/pkg/deploy/cloudformation"
	"github.com/aws/copilot-cli/internal/pkg/exec"
	"github.com/aws/copilot-cli/internal/pkg/initialize"
	"github.com/aws/copilot-cli/internal/pkg/manifest"
	"github.com/aws/copilot-cli/internal/pkg/manifest/manifestinfo"
	"github.com/aws/copilot-cli/internal/pkg/term/color"
	"github.com/aws/copilot-cli/internal/pkg/term/log"
	termprogress "github.com/aws/copilot-cli/internal/pkg/term/progress"
	"github.com/aws/copilot-cli/internal/pkg/term/prompt"
	"github.com/aws/copilot-cli/internal/pkg/term/selector"
	"github.com/aws/copilot-cli/internal/pkg/version"
	"github.com/aws/copilot-cli/internal/pkg/workspace"
)

const (
	svcWkldType = "svc"
	jobWkldType = "job"
)

type deployVars struct {
	deployWkldVars

	workloadNames []string

	yesInitWkld *bool
	deployEnv   *bool
	yesInitEnv  *bool

	region    string
	tempCreds tempCredsVars
	profile   string
}

type deployOpts struct {
	deployVars

	newWorkloadAdder func() wkldInitializerWithoutManifest
	setupDeployCmd   func(*deployOpts, string, string) (actionCommand, error)

	newInitEnvCmd   func(o *deployOpts) (cmd, error)
	newDeployEnvCmd func(o *deployOpts) (cmd, error)

	sel    wsSelector
	store  store
	ws     wsWlDirReader
	prompt prompter

	// values for logging
	wlType string

	// values for initialization logic
	envExistsInApp bool
	envExistsInWs  bool

	// Cached variables
	wsEnvironments []string
}

func newDeployOpts(vars deployVars) (*deployOpts, error) {
	sessProvider := sessions.ImmutableProvider(sessions.UserAgentExtras("deploy"))
	defaultSess, err := sessProvider.Default()
	if err != nil {
		return nil, fmt.Errorf("default session: %v", err)
	}
	store := config.NewSSMStore(identity.New(defaultSess), ssm.New(defaultSess), aws.StringValue(defaultSess.Config.Region))
	ws, err := workspace.Use(afero.NewOsFs())
	if err != nil {
		return nil, err
	}
	prompter := prompt.New()
	return &deployOpts{
		deployVars: vars,
		store:      store,
		sel:        selector.NewLocalWorkloadSelector(prompter, store, ws),
		ws:         ws,
		prompt:     prompter,

		newWorkloadAdder: func() wkldInitializerWithoutManifest {
			return &initialize.WorkloadInitializer{
				Store:    store,
				Deployer: cloudformation.New(defaultSess),
				Ws:       ws,
				Prog:     termprogress.NewSpinner(log.DiagnosticWriter),
			}
		},
		newDeployEnvCmd: func(o *deployOpts) (cmd, error) {
			// This command passes flags down from
			return newEnvDeployOpts(deployEnvVars{
				appName:           o.appName,
				name:              o.envName,
				forceNewUpdate:    o.forceNewUpdate,
				disableRollback:   o.disableRollback,
				showDiff:          o.showDiff,
				skipDiffPrompt:    o.skipDiffPrompt,
				allowEnvDowngrade: o.allowWkldDowngrade,
				detach:            o.detach,
			})
		},

		newInitEnvCmd: func(o *deployOpts) (cmd, error) {
			// This vars struct sets "default config" so that no vpc questions are asked during env init and the manifest
			// is not written. It passes in credential flags and allow-downgrade from the parent command.
			return newInitEnvOpts(initEnvVars{
				appName:           o.appName,
				name:              o.envName,
				profile:           o.profile,
				defaultConfig:     true,
				allowAppDowngrade: o.allowWkldDowngrade,
				tempCreds:         o.tempCreds,
				region:            o.region,
			})
		},

		setupDeployCmd: func(o *deployOpts, workloadName, workloadType string) (actionCommand, error) {
			switch {
			case slices.Contains(manifestinfo.JobTypes(), workloadType):
				opts := &deployJobOpts{
					deployWkldVars: o.deployWkldVars,

					store:           o.store,
					ws:              o.ws,
					newInterpolator: newManifestInterpolator,
					unmarshal:       manifest.UnmarshalWorkload,
					sel:             selector.NewLocalWorkloadSelector(o.prompt, o.store, ws),
					cmd:             exec.NewCmd(),
					templateVersion: version.LatestTemplateVersion(),
					sessProvider:    sessProvider,
				}
				opts.newJobDeployer = func() (workloadDeployer, error) {
					return newJobDeployer(opts)
				}
				opts.name = workloadName
				return opts, nil
			case slices.Contains(manifestinfo.JobTypes(), workloadType):
				opts := &deploySvcOpts{
					deployWkldVars: o.deployWkldVars,

					store:           o.store,
					ws:              o.ws,
					newInterpolator: newManifestInterpolator,
					unmarshal:       manifest.UnmarshalWorkload,
					spinner:         termprogress.NewSpinner(log.DiagnosticWriter),
					sel:             selector.NewLocalWorkloadSelector(o.prompt, o.store, ws),
					prompt:          o.prompt,
					cmd:             exec.NewCmd(),
					sessProvider:    sessProvider,
					templateVersion: version.LatestTemplateVersion(),
				}
				opts.newSvcDeployer = func() (workloadDeployer, error) {
					return newSvcDeployer(opts)
				}
				opts.name = workloadName
				return opts, nil
			}
			return nil, fmt.Errorf("unrecognized workload type %s", workloadType)
		},
	}, nil
}

func (o *deployOpts) maybeInitWkld(name string) error {
	// Confirm that the workload needs to be initialized after asking for the name.
	initializedWorkloads, err := o.store.ListWorkloads(o.appName)
	if err != nil {
		return fmt.Errorf("retrieve workloads: %w", err)
	}
	wlNames := make([]string, len(initializedWorkloads))
	for i := range initializedWorkloads {
		wlNames[i] = initializedWorkloads[i].Name
	}

	// Workload is already initialized. Return early.
	if slices.Contains(wlNames, name) {
		return nil
	}

	// Get workload type and confirm readable manifest.
	mf, err := o.ws.ReadWorkloadManifest(name)
	if err != nil {
		return fmt.Errorf("read manifest for workload %s: %w", name, err)
	}
	workloadType, err := mf.WorkloadType()
	if err != nil {
		return fmt.Errorf("get workload type from manifest for workload %s: %w", name, err)
	}

	if !slices.Contains(manifestinfo.WorkloadTypes(), workloadType) {
		return fmt.Errorf("unrecognized workload type %q in manifest for workload %s", workloadType, name)
	}

	if o.yesInitWkld == nil {
		confirmInitWkld, err := o.prompt.Confirm(fmt.Sprintf("Found manifest for uninitialized %s %q. Initialize it?", workloadType, name), "This workload will be initialized, then deployed.", prompt.WithFinalMessage(fmt.Sprintf("Initialize %s:", workloadType)))
		if err != nil {
			return fmt.Errorf("confirm initialize workload: %w", err)
		}
		o.yesInitWkld = aws.Bool(confirmInitWkld)
	}

	if !aws.BoolValue(o.yesInitWkld) {
		return fmt.Errorf("workload %s is uninitialized but --%s=false was specified", name, yesInitWorkloadFlag)
	}

	wkldAdder := o.newWorkloadAdder()
	if err = wkldAdder.AddWorkloadToApp(o.appName, name, workloadType); err != nil {
		return fmt.Errorf("add workload to app: %w", err)
	}
	return nil
}

func (o *deployOpts) Run() error {
	if err := o.askName(); err != nil {
		return err
	}

	if err := o.askEnv(); err != nil {
		return err
	}

	if err := o.checkEnvExists(); err != nil {
		return err
	}

	if err := o.maybeInitEnv(); err != nil {
		return err
	}

	if err := o.maybeDeployEnv(); err != nil {
		return err
	}

	for _, workload := range o.workloadNames {
		if err := o.maybeInitWkld(workload); err != nil {
			return err
		}
		deployCmd, err := o.loadWkldCmd(workload)
		if err != nil {
			return err
		}
		if err := deployCmd.Ask(); err != nil {
			return fmt.Errorf("ask %s deploy: %w", o.wlType, err)
		}
		if err := deployCmd.Validate(); err != nil {
			return fmt.Errorf("validate %s deploy: %w", o.wlType, err)
		}
		if err := deployCmd.Execute(); err != nil {
			return fmt.Errorf("execute %s deploy: %w", o.wlType, err)
		}
		if err := deployCmd.RecommendActions(); err != nil {
			return err
		}
	}

	return nil
}

func (o *deployOpts) askName() error {
	if o.workloadNames != nil || len(o.workloadNames) != 0 {
		return nil
	}
	name, err := o.sel.Workload("Select a service or job in your workspace", "")
	if err != nil {
		return fmt.Errorf("select service or job: %w", err)
	}
	o.workloadNames = []string{name}
	return nil
}

func (o *deployOpts) listWsEnvironments() ([]string, error) {
	if o.wsEnvironments == nil {
		envs, err := o.ws.ListEnvironments()
		if err != nil {
			return nil, err
		}
		if len(envs) == 0 {
			envs = []string{}
		}
		o.wsEnvironments = envs
		return o.wsEnvironments, nil
	}
	return o.wsEnvironments, nil
}

func (o *deployOpts) askEnv() error {
	if o.envName != "" {
		return nil
	}
	localEnvs, err := o.listWsEnvironments()
	if err != nil {
		return fmt.Errorf("get workspace environments: %w", err)
	}
	initializedEnvs, err := o.store.ListEnvironments(o.appName)
	if err != nil {
		return fmt.Errorf("get initialized environments: %w", err)
	}

	// Get uninitialized local environments and append them to the env selector call.
	var extraOptions []prompt.Option
	for _, localEnv := range localEnvs {
		var envIsInitted bool
		for _, inittedEnv := range initializedEnvs {
			if inittedEnv.Name == localEnv {
				envIsInitted = true
				break
			}
		}
		if envIsInitted {
			continue
		}
		extraOptions = append(extraOptions, prompt.Option{Value: localEnv, Hint: "uninitialized"})
	}

	o.envName, err = o.sel.Environment("Select an environment to deploy to", "", o.appName, extraOptions...)
	if err != nil {
		return fmt.Errorf("get environment name: %w", err)
	}
	return nil
}

// checkEnvExists checks whether the environment is initialized and has a local manifest.
func (o *deployOpts) checkEnvExists() error {
	o.envExistsInApp = true
	_, err := o.store.GetEnvironment(o.appName, o.envName)
	if err != nil {
		var errNotFound *config.ErrNoSuchEnvironment
		if !errors.As(err, &errNotFound) {
			return fmt.Errorf("get environment from config store: %w", err)
		}
		o.envExistsInApp = false
	}
	envs, err := o.listWsEnvironments()
	if err != nil {
		return fmt.Errorf("list environments in workspace: %w", err)
	}
	o.envExistsInWs = slices.Contains(envs, o.envName)

	// the desired environment doesn't actually exist.
	if !o.envExistsInApp && !o.envExistsInWs {
		log.Errorf("Environment %q does not exist in the current application or workspace. Please initialize it by running %s.\n", o.envName, color.HighlightCode("copilot env init"))
		return fmt.Errorf("environment %q does not exist in the workspace", o.envName)
	}
	if o.envExistsInApp && !o.envExistsInWs {
		log.Infof("Manifest for environment %q does not exist in the current workspace. To deploy this environment, generate a manifest with %s", o.envName, color.HighlightCode("copilot env show --manifest"))
	}

	return nil
}

func (o *deployOpts) maybeInitEnv() error {
	if o.envExistsInApp {
		return nil
	}

	// If no initialization flags were specified and the env wasn't initialized, ask to confirm.
	if !o.envExistsInApp && o.yesInitEnv == nil {
		v, err := o.prompt.Confirm(fmt.Sprintf("Environment %q does not exist in app %q. Initialize it?", o.envName, o.appName), "")
		if err != nil {
			return fmt.Errorf("confirm env init: %w", err)
		}
		o.yesInitEnv = aws.Bool(v)
	}

	if aws.BoolValue(o.yesInitEnv) {
		cmd, err := o.newInitEnvCmd(o)
		if err != nil {
			return fmt.Errorf("load env init command : %w", err)
		}
		if err = cmd.Validate(); err != nil {
			return err
		}
		if err = cmd.Ask(); err != nil {
			return err
		}
		if err = cmd.Execute(); err != nil {
			return err
		}
		if o.deployEnv == nil {
			log.Infof("Environment %q was just initialized. We'll deploy it now.\n", o.envName)
			o.deployEnv = aws.Bool(true)
		} else if !aws.BoolValue(o.deployEnv) {
			log.Errorf("Environment is not deployed but --%s=false was specified. Deploy the environment with %s in order to deploy a workload to it.\n", deployEnvFlag, color.HighlightCode("copilot env deploy"))
			return fmt.Errorf("environment %s was initialized but has not been deployed", o.envName)
		}
		return nil
	}
	log.Errorf("Environment %q does not exist in application %q and was not initialized after prompting.\n", o.envName, o.appName)
	return fmt.Errorf("env %s does not exist in app %s", o.envName, o.appName)
}

func (o *deployOpts) maybeDeployEnv() error {
	if !o.envExistsInWs {
		return nil
	}

	if aws.BoolValue(o.deployEnv) {
		cmd, err := o.newDeployEnvCmd(o)
		if err != nil {
			return fmt.Errorf("set up env deploy command: %w", err)
		}
		if err = cmd.Validate(); err != nil {
			return err
		}
		if err = cmd.Ask(); err != nil {
			return err
		}
		return cmd.Execute()
	}
	return nil
}

func (o *deployOpts) loadWkldCmd(name string) (actionCommand, error) {
	wl, err := o.store.GetWorkload(o.appName, name)
	if err != nil {
		return nil, fmt.Errorf("retrieve %s from application %s: %w", o.appName, name, err)
	}
	cmd, err := o.setupDeployCmd(o, name, wl.Type)
	if err != nil {
		return nil, err
	}
	if slices.Contains(manifestinfo.JobTypes(), wl.Type) {
		o.wlType = jobWkldType
		return cmd, nil
	}
	o.wlType = svcWkldType
	return cmd, nil
}

// BuildDeployCmd is the deploy command.
func BuildDeployCmd() *cobra.Command {
	vars := deployVars{}
	var initWorkload bool
	var initEnvironment bool
	var deployEnvironment bool
	var name string
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy a Copilot job or service.",
		Long:  "Deploy a Copilot job or service.",
		Example: `
  Deploys a service named "frontend" to a "test" environment.
  /code $ copilot deploy --name frontend --env test --deploy-env=false
  Deploys a job named "mailer" with additional resource tags to a "prod" environment.
  /code $ copilot deploy -n mailer -e prod --resource-tags source/revision=bb133e7,deployment/initiator=manual --deploy-env=false
  Initializes and deploys an environment named "test" in us-west-2 under the "default" profile with local manifest, 
    then deploys a service named "api"
  /code $ copilot deploy --init-env --deploy-env --env test --name api --profile default --region us-west-2
  Initializes and deploys a service named "backend" to a "prod" environment.
  /code $ copilot deploy --init-wkld --deploy-env=false --env prod --name backend`,

		RunE: runCmdE(func(cmd *cobra.Command, args []string) error {
			opts, err := newDeployOpts(vars)
			if err != nil {
				return err
			}

			if cmd.Flags().Changed(yesInitWorkloadFlag) {
				opts.yesInitWkld = aws.Bool(false)
				if initWorkload {
					opts.yesInitWkld = aws.Bool(true)
				}
			}

			if cmd.Flags().Changed(yesInitEnvFlag) {
				opts.yesInitEnv = aws.Bool(false)
				if initEnvironment {
					opts.yesInitEnv = aws.Bool(true)
				}
			}

			if cmd.Flags().Changed(deployEnvFlag) {
				opts.deployEnv = aws.Bool(false)
				if deployEnvironment {
					opts.deployEnv = aws.Bool(true)
				}
			}

			if cmd.Flags().Changed(nameFlag) {
				opts.workloadNames = []string{name}
			}

			if err := opts.Run(); err != nil {
				return err
			}
			return nil
		}),
	}
	cmd.Flags().StringVarP(&vars.appName, appFlag, appFlagShort, tryReadingAppName(), appFlagDescription)
	cmd.Flags().StringVarP(&name, nameFlag, nameFlagShort, "", workloadFlagDescription)
	cmd.Flags().StringVarP(&vars.envName, envFlag, envFlagShort, "", envFlagDescription)
	cmd.Flags().StringVar(&vars.imageTag, imageTagFlag, "", imageTagFlagDescription)
	cmd.Flags().StringToStringVar(&vars.resourceTags, resourceTagsFlag, nil, resourceTagsFlagDescription)
	cmd.Flags().BoolVar(&vars.forceNewUpdate, forceFlag, false, forceFlagDescription)
	cmd.Flags().BoolVar(&vars.disableRollback, noRollbackFlag, false, noRollbackFlagDescription)
	cmd.Flags().BoolVar(&vars.allowWkldDowngrade, allowDowngradeFlag, false, allowDowngradeFlagDescription)
	cmd.Flags().BoolVar(&vars.detach, detachFlag, false, detachFlagDescription)

	cmd.Flags().BoolVar(&deployEnvironment, deployEnvFlag, false, deployEnvFlagDescription)
	cmd.Flags().BoolVar(&initEnvironment, yesInitEnvFlag, false, yesInitEnvFlagDescription)
	cmd.Flags().BoolVar(&initWorkload, yesInitWorkloadFlag, false, yesInitWorkloadFlagDescription)

	cmd.Flags().StringVar(&vars.profile, profileFlag, "", profileFlagDescription)
	cmd.Flags().StringVar(&vars.tempCreds.AccessKeyID, accessKeyIDFlag, "", accessKeyIDFlagDescription)
	cmd.Flags().StringVar(&vars.tempCreds.SecretAccessKey, secretAccessKeyFlag, "", secretAccessKeyFlagDescription)
	cmd.Flags().StringVar(&vars.tempCreds.SessionToken, sessionTokenFlag, "", sessionTokenFlagDescription)
	cmd.Flags().StringVar(&vars.region, regionFlag, "", envRegionTokenFlagDescription)

	cmd.SetUsageTemplate(template.Usage)
	cmd.Annotations = map[string]string{
		"group": group.Release,
	}
	return cmd
}
