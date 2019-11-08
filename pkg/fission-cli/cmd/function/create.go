/*
Copyright 2019 The Fission Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package function

import (
	"fmt"
	"strings"

	"github.com/pkg/errors"
	uuid "github.com/satori/go.uuid"
	apiv1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fv1 "github.com/fission/fission/pkg/apis/fission.io/v1"
	"github.com/fission/fission/pkg/controller/client"
	ferror "github.com/fission/fission/pkg/error"
	"github.com/fission/fission/pkg/fission-cli/cliwrapper/cli"
	"github.com/fission/fission/pkg/fission-cli/cmd/httptrigger"
	_package "github.com/fission/fission/pkg/fission-cli/cmd/package"
	"github.com/fission/fission/pkg/fission-cli/cmd/spec"
	"github.com/fission/fission/pkg/fission-cli/console"
	flagkey "github.com/fission/fission/pkg/fission-cli/flag/key"
	"github.com/fission/fission/pkg/fission-cli/util"
	"github.com/fission/fission/pkg/types"
)

const (
	DEFAULT_MIN_SCALE             = 1
	DEFAULT_TARGET_CPU_PERCENTAGE = 80
)

type CreateSubCommand struct {
	client   *client.Client
	function *fv1.Function
	specFile string
}

func Create(input cli.Input) error {
	c, err := util.GetServer(input)
	if err != nil {
		return err
	}
	opts := CreateSubCommand{
		client: c,
	}
	return opts.do(input)
}

func (opts *CreateSubCommand) do(input cli.Input) error {
	err := opts.complete(input)
	if err != nil {
		return err
	}
	return opts.run(input)
}

func (opts *CreateSubCommand) complete(input cli.Input) error {
	fnName := input.String(flagkey.FnName)
	fnNamespace := input.String(flagkey.NamespaceFunction)
	envNamespace := input.String(flagkey.NamespaceEnvironment)

	// user wants a spec, create a yaml file with package and function
	toSpec := false
	if input.Bool(flagkey.SpecSave) {
		toSpec = true
		opts.specFile = fmt.Sprintf("function-%v.yaml", fnName)
	}
	specDir := util.GetSpecDir(input)

	// check for unique function names within a namespace
	metadata, err := util.GetMetadata(flagkey.FnName, flagkey.NamespaceFunction, input)
	if err != nil {
		return err
	}

	fn, err := opts.client.FunctionGet(metadata)
	if err != nil && !ferror.IsNotFound(err) {
		return err
	} else if fn != nil {
		return errors.New("a function with the same name already exists")
	}

	entrypoint := input.String(flagkey.FnEntrypoint)

	fnTimeout := input.Int(flagkey.FnExecutionTimeout)
	if fnTimeout <= 0 {
		return errors.New("fntimeout must be greater than 0")
	}

	pkgName := input.String(flagkey.FnPackageName)

	secretNames := input.StringSlice(flagkey.FnSecret)
	cfgMapNames := input.StringSlice(flagkey.FnCfgMap)

	invokeStrategy, err := getInvokeStrategy(input, nil)
	if err != nil {
		return err
	}
	resourceReq, err := util.GetResourceReqs(input, &apiv1.ResourceRequirements{})
	if err != nil {
		return err
	}

	var pkgMetadata *metav1.ObjectMeta
	var envName string
	if len(pkgName) > 0 {
		// use existing package
		pkg, err := opts.client.PackageGet(&metav1.ObjectMeta{
			Namespace: fnNamespace,
			Name:      pkgName,
		})
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("read package in '%v' in Namespace: %s. Package needs to be present in the same namespace as function", pkgName, fnNamespace))
		}
		pkgMetadata = &pkg.Metadata
		envName = pkg.Spec.Environment.Name
		if envName != input.String(flagkey.FnEnvironmentName) {
			console.Warn("Function's environment is different than package's environment, package's environment will be used for creating function")
		}
		envNamespace = pkg.Spec.Environment.Namespace
	} else {
		// need to specify environment for creating new package
		envName = input.String(flagkey.FnEnvironmentName)
		if len(envName) == 0 {
			return errors.New("need --env argument")
		}

		// examine existence of given environment. If specs - then spec validate will do it, don't check here.
		if !toSpec {
			_, err := opts.client.EnvironmentGet(&metav1.ObjectMeta{
				Namespace: envNamespace,
				Name:      envName,
			})
			if err != nil {
				if e, ok := err.(ferror.Error); ok && e.Code == ferror.ErrorNotFound {
					console.Warn(fmt.Sprintf("Environment \"%v\" does not exist. Please create the environment before executing the function. \nFor example: `fission env create --name %v --envns %v --image <image>`\n", envName, envName, envNamespace))
				} else {
					return errors.Wrap(err, "error retrieving environment information")
				}
			}
		}

		srcArchiveFiles := input.StringSlice(flagkey.PkgSrcArchive)
		var deployArchiveFiles []string
		noZip := false
		code := input.String(flagkey.FnCode)
		if len(code) == 0 {
			deployArchiveFiles = input.StringSlice(flagkey.PkgDeployArchive)
		} else {
			deployArchiveFiles = append(deployArchiveFiles, input.String(flagkey.FnCode))
			noZip = true
		}
		// return error when both src & deploy archive are empty
		if len(srcArchiveFiles) == 0 && len(deployArchiveFiles) == 0 {
			return errors.New("need --code or --deploy or --src argument")
		}

		buildcmd := input.String(flagkey.PkgBuildCmd)
		keepURL := input.Bool(flagkey.PkgKeepURL)

		// create new package in the same namespace as the function.
		pkgMetadata, err = _package.CreatePackage(input, opts.client, fnNamespace, envName, envNamespace,
			srcArchiveFiles, deployArchiveFiles, buildcmd, specDir, opts.specFile, noZip, keepURL)
		if err != nil {
			return errors.Wrap(err, "error creating package")
		}
	}

	var secrets []fv1.SecretReference
	var cfgmaps []fv1.ConfigMapReference

	if len(secretNames) > 0 {
		// check the referenced secret is in the same ns as the function, if not give a warning.
		for _, secretName := range secretNames {
			_, err := opts.client.SecretGet(&metav1.ObjectMeta{
				Namespace: fnNamespace,
				Name:      secretName,
			})
			if err != nil {
				if k8serrors.IsNotFound(err) {
					console.Warn(fmt.Sprintf("Secret %s not found in Namespace: %s. Secret needs to be present in the same namespace as function", secretName, fnNamespace))
				} else {
					return errors.Wrap(err, "error checking secret")
				}
			}
		}
		for _, secretName := range secretNames {
			newSecret := fv1.SecretReference{
				Name:      secretName,
				Namespace: fnNamespace,
			}
			secrets = append(secrets, newSecret)
		}
	}

	if len(cfgMapNames) > 0 {
		// check the referenced cfgmap is in the same ns as the function, if not give a warning.
		for _, cfgMapName := range cfgMapNames {
			_, err := opts.client.ConfigMapGet(&metav1.ObjectMeta{
				Namespace: fnNamespace,
				Name:      cfgMapName,
			})
			if err != nil {
				if k8serrors.IsNotFound(err) {
					console.Warn(fmt.Sprintf("ConfigMap %s not found in Namespace: %s. ConfigMap needs to be present in the same namespace as function", cfgMapName, fnNamespace))
				} else {
					return errors.Wrap(err, "error checking configmap")
				}
			}
		}
		for _, cfgMapName := range cfgMapNames {
			newCfgMap := fv1.ConfigMapReference{
				Name:      cfgMapName,
				Namespace: fnNamespace,
			}
			cfgmaps = append(cfgmaps, newCfgMap)
		}
	}

	opts.function = &fv1.Function{
		Metadata: metav1.ObjectMeta{
			Name:      fnName,
			Namespace: fnNamespace,
		},
		Spec: fv1.FunctionSpec{
			Environment: fv1.EnvironmentReference{
				Name:      envName,
				Namespace: envNamespace,
			},
			Package: fv1.FunctionPackageRef{
				FunctionName: entrypoint,
				PackageRef: fv1.PackageRef{
					Namespace:       pkgMetadata.Namespace,
					Name:            pkgMetadata.Name,
					ResourceVersion: pkgMetadata.ResourceVersion,
				},
			},
			Secrets:         secrets,
			ConfigMaps:      cfgmaps,
			Resources:       *resourceReq,
			InvokeStrategy:  *invokeStrategy,
			FunctionTimeout: fnTimeout,
		},
	}

	return nil
}

// run write the resource to a spec file or create a fission CRD with remote fission server.
// It also prints warning/error if necessary.
func (opts *CreateSubCommand) run(flags cli.Input) error {
	// if we're writing a spec, don't create the function
	if flags.Bool(flagkey.SpecSave) {
		err := spec.SpecSave(*opts.function, opts.specFile)
		if err != nil {
			return errors.Wrap(err, "error creating function spec")
		}
		return nil
	}

	_, err := opts.client.FunctionCreate(opts.function)
	if err != nil {
		return errors.Wrap(err, "error creating function")
	}

	fmt.Printf("function '%v' created\n", opts.function.Metadata.Name)

	// Allow the user to specify an HTTP trigger while creating a function.
	triggerUrl := flags.String(flagkey.HtUrl)
	if len(triggerUrl) == 0 {
		return nil
	}
	if !strings.HasPrefix(triggerUrl, "/") {
		triggerUrl = fmt.Sprintf("/%s", triggerUrl)
	}

	method, err := httptrigger.GetMethod(flags.String(flagkey.HtMethod))
	if err != nil {
		return errors.Wrap(err, "error getting HTTP trigger method")
	}

	triggerName := uuid.NewV4().String()
	ht := &fv1.HTTPTrigger{
		Metadata: metav1.ObjectMeta{
			Name:      triggerName,
			Namespace: opts.function.Metadata.Namespace,
		},
		Spec: fv1.HTTPTriggerSpec{
			RelativeURL: triggerUrl,
			Method:      method,
			FunctionReference: fv1.FunctionReference{
				Type: fv1.FunctionReferenceTypeFunctionName,
				Name: opts.function.Metadata.Name,
			},
		},
	}
	_, err = opts.client.HTTPTriggerCreate(ht)
	if err != nil {
		return errors.Wrap(err, "error creating HTTP trigger")
	}

	fmt.Printf("route created: %v %v -> %v\n", method, triggerUrl, opts.function.Metadata.Name)
	return nil
}

func getInvokeStrategy(flags cli.Input, existingInvokeStrategy *fv1.InvokeStrategy) (strategy *fv1.InvokeStrategy, err error) {

	var fnExecutor, newFnExecutor fv1.ExecutorType

	switch flags.String(flagkey.FnExecutorType) {
	case "":
		fallthrough
	case types.ExecutorTypePoolmgr:
		newFnExecutor = types.ExecutorTypePoolmgr
	case types.ExecutorTypeNewdeploy:
		newFnExecutor = types.ExecutorTypeNewdeploy
	default:
		return nil, errors.New("executor type must be one of 'poolmgr' or 'newdeploy', defaults to 'poolmgr'")
	}

	if existingInvokeStrategy != nil {
		fnExecutor = existingInvokeStrategy.ExecutionStrategy.ExecutorType

		// override the executor type if user specified a new executor type
		if flags.IsSet(flagkey.FnExecutorType) {
			fnExecutor = newFnExecutor
		}
	} else {
		fnExecutor = newFnExecutor
	}

	if flags.IsSet(flagkey.FnSpecializationTimeout) && fnExecutor != types.ExecutorTypeNewdeploy {
		return nil, errors.Errorf("%v flag is only applicable for newdeploy type of executor", flagkey.FnSpecializationTimeout)
	}

	if fnExecutor == types.ExecutorTypePoolmgr {
		if flags.IsSet(flagkey.RuntimeTargetcpu) || flags.IsSet(flagkey.ReplicasMinscale) || flags.IsSet(flagkey.ReplicasMaxscale) {
			return nil, errors.New("to set target CPU or min/max scale for function, please specify \"--executortype newdeploy\"")
		}

		if flags.IsSet(flagkey.RuntimeMincpu) || flags.IsSet(flagkey.RuntimeMaxcpu) || flags.IsSet(flagkey.RuntimeMinmemory) || flags.IsSet(flagkey.RuntimeMaxmemory) {
			console.Warn("To limit CPU/Memory for function with executor type \"poolmgr\", please specify resources limits when creating environment")
		}
		strategy = &fv1.InvokeStrategy{
			StrategyType: fv1.StrategyTypeExecution,
			ExecutionStrategy: fv1.ExecutionStrategy{
				ExecutorType: types.ExecutorTypePoolmgr,
			},
		}
	} else {
		// set default value
		targetCPU := DEFAULT_TARGET_CPU_PERCENTAGE
		minScale := DEFAULT_MIN_SCALE
		maxScale := minScale
		specializationTimeout := fv1.DefaultSpecializationTimeOut

		if existingInvokeStrategy != nil && existingInvokeStrategy.ExecutionStrategy.ExecutorType == types.ExecutorTypeNewdeploy {
			minScale = existingInvokeStrategy.ExecutionStrategy.MinScale
			maxScale = existingInvokeStrategy.ExecutionStrategy.MaxScale
			targetCPU = existingInvokeStrategy.ExecutionStrategy.TargetCPUPercent
			specializationTimeout = existingInvokeStrategy.ExecutionStrategy.SpecializationTimeout
		}

		if flags.IsSet(flagkey.RuntimeTargetcpu) {
			targetCPU, err = getTargetCPU(flags)
			if err != nil {
				return nil, err
			}
		}

		if flags.IsSet(flagkey.ReplicasMinscale) {
			minScale = flags.Int(flagkey.ReplicasMinscale)
		}

		if flags.IsSet(flagkey.ReplicasMaxscale) {
			maxScale = flags.Int(flagkey.ReplicasMaxscale)
			if maxScale <= 0 {
				return nil, errors.Errorf("%v must be greater than 0", flagkey.ReplicasMaxscale)
			}
		}

		if flags.IsSet(flagkey.FnSpecializationTimeout) {
			specializationTimeout = flags.Int(flagkey.FnSpecializationTimeout)
			if specializationTimeout < fv1.DefaultSpecializationTimeOut {
				return nil, errors.Errorf("%v must be greater than or equal to 120 seconds", flagkey.FnSpecializationTimeout)
			}
		}

		if minScale > maxScale {
			return nil, fmt.Errorf("minscale (%v) can not be greater than maxscale (%v)", minScale, maxScale)
		}

		// Right now a simple single case strategy implementation
		// This will potentially get more sophisticated once we have more strategies in place
		strategy = &fv1.InvokeStrategy{
			StrategyType: fv1.StrategyTypeExecution,
			ExecutionStrategy: fv1.ExecutionStrategy{
				ExecutorType:          fnExecutor,
				MinScale:              minScale,
				MaxScale:              maxScale,
				TargetCPUPercent:      targetCPU,
				SpecializationTimeout: specializationTimeout,
			},
		}
	}

	return strategy, nil
}

func getTargetCPU(flags cli.Input) (int, error) {
	targetCPU := flags.Int(flagkey.RuntimeTargetcpu)
	if targetCPU <= 0 || targetCPU > 100 {
		return 0, errors.Errorf("%v must be a value between 1 - 100", flagkey.RuntimeTargetcpu)
	}
	return targetCPU, nil
}