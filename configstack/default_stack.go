package configstack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gruntwork-io/go-commons/collections"
	"github.com/gruntwork-io/terragrunt/cli/commands/run/creds"
	"github.com/gruntwork-io/terragrunt/cli/commands/run/creds/providers/externalcmd"
	"github.com/gruntwork-io/terragrunt/config/hclparse"
	"github.com/gruntwork-io/terragrunt/pkg/log"
	"github.com/gruntwork-io/terragrunt/telemetry"
	"github.com/gruntwork-io/terragrunt/tf"

	"github.com/gruntwork-io/terragrunt/config"
	"github.com/gruntwork-io/terragrunt/internal/errors"
	"github.com/gruntwork-io/terragrunt/internal/report"
	"github.com/gruntwork-io/terragrunt/options"
	"github.com/gruntwork-io/terragrunt/util"
)

// DefaultStack implements the Stack interface and represents a stack of Terraform modules (i.e. folders with Terraform templates) that you can "spin up" or "spin down" in a single command
// (formerly Stack)
type DefaultStack struct {
	report                *report.Report
	parserOptions         []hclparse.Option
	terragruntOptions     *options.TerragruntOptions
	childTerragruntConfig *config.TerragruntConfig
	modules               TerraformModules
	outputMu              sync.Mutex
}

// NewDefaultStack creates a new DefaultStack.
func NewDefaultStack(l log.Logger, terragruntOptions *options.TerragruntOptions, opts ...Option) *DefaultStack {
	stack := &DefaultStack{
		terragruntOptions: terragruntOptions,
		parserOptions:     config.DefaultParserOptions(l, terragruntOptions),
	}

	return stack.WithOptions(opts...)
}

// WithOptions updates the stack with the provided options.
func (stack *DefaultStack) WithOptions(opts ...Option) *DefaultStack {
	for _, opt := range opts {
		opt(stack)
	}

	return stack
}

// SetReport sets the report for the stack.
func (stack *DefaultStack) SetReport(report *report.Report) {
	stack.report = report
}

// GetReport returns the report for the stack.
func (stack *DefaultStack) GetReport() *report.Report {
	return stack.report
}

// String renders this stack as a human-readable string
func (stack *DefaultStack) String() string {
	modules := []string{}
	for _, module := range stack.modules {
		modules = append(modules, "  => "+module.String())
	}

	sort.Strings(modules)

	return fmt.Sprintf("Stack at %s:\n%s", stack.terragruntOptions.WorkingDir, strings.Join(modules, "\n"))
}

// LogModuleDeployOrder will log the modules that will be deployed by this operation, in the order that the operations
// happen. For plan and apply, the order will be bottom to top (dependencies first), while for destroy the order will be
// in reverse.
func (stack *DefaultStack) LogModuleDeployOrder(l log.Logger, terraformCommand string) error {
	outStr := fmt.Sprintf("The stack at %s will be processed in the following order for command %s:\n", stack.terragruntOptions.WorkingDir, terraformCommand)

	runGraph, err := stack.GetModuleRunGraph(terraformCommand)
	if err != nil {
		return err
	}

	for i, group := range runGraph {
		outStr += fmt.Sprintf("Group %d\n", i+1)
		for _, module := range group {
			outStr += fmt.Sprintf("- Module %s\n", module.Path)
		}

		outStr += "\n"
	}

	l.Info(outStr)

	return nil
}

// JSONModuleDeployOrder will return the modules that will be deployed by a plan/apply operation, in the order
// that the operations happen.
func (stack *DefaultStack) JSONModuleDeployOrder(terraformCommand string) (string, error) {
	runGraph, err := stack.GetModuleRunGraph(terraformCommand)
	if err != nil {
		return "", errors.New(err)
	}

	// Convert the module paths to a string array for JSON marshalling
	// The index should be the group number, and the value should be an array of module paths
	jsonGraph := make(map[string][]string)

	for i, group := range runGraph {
		groupNum := "Group " + strconv.Itoa(i+1)
		jsonGraph[groupNum] = make([]string, len(group))

		for j, module := range group {
			jsonGraph[groupNum][j] = module.Path
		}
	}

	j, err := json.MarshalIndent(jsonGraph, "", "  ")
	if err != nil {
		return "", errors.New(err)
	}

	return string(j), nil
}

// Graph creates a graphviz representation of the modules
func (stack *DefaultStack) Graph(l log.Logger, opts *options.TerragruntOptions) {
	err := stack.modules.WriteDot(l, opts.Writer, opts)
	if err != nil {
		l.Warnf("Failed to graph dot: %v", err)
	}
}

func (stack *DefaultStack) Run(ctx context.Context, l log.Logger, opts *options.TerragruntOptions) error {
	stackCmd := opts.TerraformCommand

	// prepare folder for output hierarchy if output folder is set
	if opts.OutputFolder != "" {
		for _, module := range stack.modules {
			planFile := module.outputFile(l, opts)

			planDir := filepath.Dir(planFile)
			if err := os.MkdirAll(planDir, os.ModePerm); err != nil {
				return err
			}
		}
	}

	// For any command that needs input, run in non-interactive mode to avoid cominglint stdin across multiple
	// concurrent runs.
	if util.ListContainsElement(config.TerraformCommandsNeedInput, stackCmd) {
		// to support potential positional args in the args list, we append the input=false arg after the first element,
		// which is the target command.
		opts.TerraformCliArgs = util.StringListInsert(opts.TerraformCliArgs, "-input=false", 1)
		stack.syncTerraformCliArgs(l, opts)
	}

	// For apply and destroy, run with auto-approve (unless explicitly disabled) due to the co-mingling of the prompts.
	// This is not ideal, but until we have a better way of handling interactivity with run --all, we take the evil of
	// having a global prompt (managed in cli/cli_app.go) be the gate keeper.
	switch stackCmd {
	case tf.CommandNameApply, tf.CommandNameDestroy:
		// to support potential positional args in the args list, we append the input=false arg after the first element,
		// which is the target command.
		if opts.RunAllAutoApprove {
			opts.TerraformCliArgs = util.StringListInsert(opts.TerraformCliArgs, "-auto-approve", 1)
		}

		stack.syncTerraformCliArgs(l, opts)
	case tf.CommandNameShow:
		stack.syncTerraformCliArgs(l, opts)
	case tf.CommandNamePlan:
		// We capture the out stream for each module
		errorStreams := make([]bytes.Buffer, len(stack.modules))

		for n, module := range stack.modules {
			module.TerragruntOptions.ErrWriter = io.MultiWriter(&errorStreams[n], module.TerragruntOptions.ErrWriter)
		}

		defer stack.summarizePlanAllErrors(l, errorStreams)
	}

	switch {
	case opts.IgnoreDependencyOrder:
		return stack.modules.RunModulesIgnoreOrder(ctx, opts, stack.report, opts.Parallelism)
	case stackCmd == tf.CommandNameDestroy:
		return stack.modules.RunModulesReverseOrder(ctx, opts, stack.report, opts.Parallelism)
	default:
		return stack.modules.RunModules(ctx, opts, stack.report, opts.Parallelism)
	}
}

// We inspect the error streams to give an explicit message if the plan failed because there were references to
// remote states. `terraform plan` will fail if it tries to access remote state from dependencies and the plan
// has never been applied on the dependency.
func (stack *DefaultStack) summarizePlanAllErrors(l log.Logger, errorStreams []bytes.Buffer) {
	for i, errorStream := range errorStreams {
		output := errorStream.String()

		if len(output) == 0 {
			// We get empty buffer if stack execution completed without errors, so skip that to avoid logging too much
			continue
		}

		if strings.Contains(output, "Error running plan:") && strings.Contains(output, ": Resource 'data.terraform_remote_state.") {
			var dependenciesMsg string
			if len(stack.modules[i].Dependencies) > 0 {
				dependenciesMsg = fmt.Sprintf(" contains dependencies to %v and", stack.modules[i].Config.Dependencies.Paths)
			}

			l.Infof("%v%v refers to remote state "+
				"you may have to apply your changes in the dependencies prior running terragrunt run --all plan.\n",
				stack.modules[i].Path,
				dependenciesMsg,
			)
		}
	}
}

// Sync the TerraformCliArgs for each module in the stack to match the provided terragruntOptions struct.
func (stack *DefaultStack) syncTerraformCliArgs(l log.Logger, opts *options.TerragruntOptions) {
	for _, module := range stack.modules {
		module.TerragruntOptions.TerraformCliArgs = collections.MakeCopyOfList(opts.TerraformCliArgs)

		planFile := module.planFile(l, opts)

		if planFile != "" {
			l.Debugf("Using output file %s for module %s", planFile, module.TerragruntOptions.TerragruntConfigPath)

			if module.TerragruntOptions.TerraformCommand == tf.CommandNamePlan {
				// for plan command add -out=<file> to the terraform cli args
				module.TerragruntOptions.TerraformCliArgs = util.StringListInsert(module.TerragruntOptions.TerraformCliArgs, "-out="+planFile, len(module.TerragruntOptions.TerraformCliArgs))
			} else {
				module.TerragruntOptions.TerraformCliArgs = util.StringListInsert(module.TerragruntOptions.TerraformCliArgs, planFile, len(module.TerragruntOptions.TerraformCliArgs))
			}
		}
	}
}

func (stack *DefaultStack) toRunningModules(terraformCommand string) (RunningModules, error) {
	switch terraformCommand {
	case tf.CommandNameDestroy:
		return stack.modules.ToRunningModules(ReverseOrder, stack.report, stack.terragruntOptions)
	default:
		return stack.modules.ToRunningModules(NormalOrder, stack.report, stack.terragruntOptions)
	}
}

// GetModuleRunGraph converts the module list to a graph that shows the order in which the modules will be
// applied/destroyed. The return structure is a list of lists, where the nested list represents modules that can be
// deployed concurrently, and the outer list indicates the order. This will only include those modules that do NOT have
// the exclude flag set.
func (stack *DefaultStack) GetModuleRunGraph(terraformCommand string) ([]TerraformModules, error) {
	moduleRunGraph, err := stack.toRunningModules(terraformCommand)
	if err != nil {
		return nil, err
	}

	// Set maxDepth for the graph so that we don't get stuck in an infinite loop.
	const maxDepth = 1000
	groups := moduleRunGraph.toTerraformModuleGroups(maxDepth)

	return groups, nil
}

// Find all the Terraform modules in the folders that contain the given Terragrunt config files and assemble those
// modules into a Stack object that can be applied or destroyed in a single command
func (stack *DefaultStack) createStackForTerragruntConfigPaths(ctx context.Context, l log.Logger, terragruntConfigPaths []string) error {
	err := telemetry.TelemeterFromContext(ctx).Collect(ctx, "create_stack_for_terragrunt_config_paths", map[string]any{
		"working_dir": stack.terragruntOptions.WorkingDir,
	}, func(ctx context.Context) error {
		if len(terragruntConfigPaths) == 0 {
			return errors.New(ErrNoTerraformModulesFound)
		}

		modules, err := stack.ResolveTerraformModules(ctx, l, terragruntConfigPaths)
		if err != nil {
			return errors.New(err)
		}

		stack.SetModules(modules)

		return nil
	})
	if err != nil {
		return errors.New(err)
	}

	err = telemetry.TelemeterFromContext(ctx).Collect(ctx, "check_for_cycles", map[string]any{
		"working_dir": stack.terragruntOptions.WorkingDir,
	}, func(_ context.Context) error {
		if err := stack.modules.CheckForCycles(); err != nil {
			return errors.New(err)
		}

		return nil
	})

	if err != nil {
		return errors.New(err)
	}

	return nil
}

// ResolveTerraformModules goes through each of the given Terragrunt configuration files
// and resolve the module that configuration file represents into a TerraformModule struct.
// Return the list of these TerraformModule structs.
func (stack *DefaultStack) ResolveTerraformModules(ctx context.Context, l log.Logger, terragruntConfigPaths []string) (TerraformModules, error) {
	canonicalTerragruntConfigPaths, err := util.CanonicalPaths(terragruntConfigPaths, ".")
	if err != nil {
		return nil, err
	}

	var modulesMap TerraformModulesMap

	err = telemetry.TelemeterFromContext(ctx).Collect(ctx, "resolve_modules", map[string]any{
		"working_dir": stack.terragruntOptions.WorkingDir,
	}, func(ctx context.Context) error {
		howThesePathsWereFound := "Terragrunt config file found in a subdirectory of " + stack.terragruntOptions.WorkingDir

		result, err := stack.resolveModules(ctx, l, canonicalTerragruntConfigPaths, howThesePathsWereFound)
		if err != nil {
			return err
		}

		modulesMap = result

		return nil
	})

	if err != nil {
		return nil, err
	}

	var externalDependencies TerraformModulesMap

	err = telemetry.TelemeterFromContext(ctx).Collect(ctx, "resolve_external_dependencies_for_modules", map[string]any{
		"working_dir": stack.terragruntOptions.WorkingDir,
	}, func(ctx context.Context) error {
		result, err := stack.resolveExternalDependenciesForModules(ctx, l, modulesMap, TerraformModulesMap{}, 0)
		if err != nil {
			return err
		}

		externalDependencies = result

		return nil
	})
	if err != nil {
		return nil, err
	}

	var crossLinkedModules TerraformModules

	err = telemetry.TelemeterFromContext(ctx).Collect(ctx, "crosslink_dependencies", map[string]any{
		"working_dir": stack.terragruntOptions.WorkingDir,
	}, func(_ context.Context) error {
		result, err := modulesMap.mergeMaps(externalDependencies).crosslinkDependencies(canonicalTerragruntConfigPaths)
		if err != nil {
			return err
		}

		crossLinkedModules = result

		return nil
	})

	if err != nil {
		return nil, err
	}

	var withUnitsIncluded TerraformModules

	err = telemetry.TelemeterFromContext(ctx).Collect(ctx, "flag_included_dirs", map[string]any{
		"working_dir": stack.terragruntOptions.WorkingDir,
	}, func(_ context.Context) error {
		withUnitsIncluded = crossLinkedModules.flagIncludedDirs(stack.terragruntOptions)
		return nil
	})

	if err != nil {
		return nil, err
	}

	var withUnitsThatAreIncludedByOthers TerraformModules

	err = telemetry.TelemeterFromContext(ctx).Collect(ctx, "flag_units_that_are_included", map[string]any{
		"working_dir": stack.terragruntOptions.WorkingDir,
	}, func(_ context.Context) error {
		result, err := withUnitsIncluded.flagUnitsThatAreIncluded(stack.terragruntOptions)
		if err != nil {
			return err
		}

		withUnitsThatAreIncludedByOthers = result

		return nil
	})

	if err != nil {
		return nil, err
	}

	var withExcludedUnits TerraformModules

	err = telemetry.TelemeterFromContext(ctx).Collect(ctx, "flag_excluded_units", map[string]any{
		"working_dir": stack.terragruntOptions.WorkingDir,
	}, func(_ context.Context) error {
		result := withUnitsThatAreIncludedByOthers.flagExcludedUnits(l, stack.terragruntOptions)
		withExcludedUnits = result

		return nil
	})

	if err != nil {
		return nil, err
	}

	var withUnitsRead TerraformModules

	err = telemetry.TelemeterFromContext(ctx).Collect(ctx, "flag_units_that_read", map[string]any{
		"working_dir": stack.terragruntOptions.WorkingDir,
	}, func(_ context.Context) error {
		withUnitsRead = withExcludedUnits.flagUnitsThatRead(stack.terragruntOptions)

		return nil
	})

	if err != nil {
		return nil, err
	}

	var withModulesExcluded TerraformModules

	err = telemetry.TelemeterFromContext(ctx).Collect(ctx, "flag_excluded_dirs", map[string]any{
		"working_dir": stack.terragruntOptions.WorkingDir,
	}, func(_ context.Context) error {
		withModulesExcluded = withUnitsRead.flagExcludedDirs(stack.terragruntOptions)
		return nil
	})

	if err != nil {
		return nil, err
	}

	return withModulesExcluded, nil
}

// Go through each of the given Terragrunt configuration files and resolve the module that configuration file represents
// into a TerraformModule struct. Note that this method will NOT fill in the Dependencies field of the TerraformModule
// struct (see the crosslinkDependencies method for that). Return a map from module path to TerraformModule struct.
func (stack *DefaultStack) resolveModules(ctx context.Context, l log.Logger, canonicalTerragruntConfigPaths []string, howTheseModulesWereFound string) (TerraformModulesMap, error) {
	modulesMap := TerraformModulesMap{}

	for _, terragruntConfigPath := range canonicalTerragruntConfigPaths {
		if !util.FileExists(terragruntConfigPath) {
			return nil, ProcessingModuleError{UnderlyingError: os.ErrNotExist, ModulePath: terragruntConfigPath, HowThisModuleWasFound: howTheseModulesWereFound}
		}

		var module *TerraformModule

		err := telemetry.TelemeterFromContext(ctx).Collect(ctx, "resolve_terraform_module", map[string]any{
			"config_path": terragruntConfigPath,
			"working_dir": stack.terragruntOptions.WorkingDir,
		}, func(ctx context.Context) error {
			m, err := stack.resolveTerraformModule(ctx, l, terragruntConfigPath, modulesMap, howTheseModulesWereFound)
			if err != nil {
				return err
			}

			module = m

			return nil
		})

		if err != nil {
			return modulesMap, err
		}

		if module != nil {
			modulesMap[module.Path] = module

			var dependencies TerraformModulesMap

			err := telemetry.TelemeterFromContext(ctx).Collect(ctx, "resolve_dependencies_for_module", map[string]any{
				"config_path": terragruntConfigPath,
				"working_dir": stack.terragruntOptions.WorkingDir,
				"module_path": module.Path,
			}, func(ctx context.Context) error {
				deps, err := stack.resolveDependenciesForModule(ctx, l, module, modulesMap, true)
				if err != nil {
					return err
				}

				dependencies = deps

				return nil
			})
			if err != nil {
				return modulesMap, err
			}

			modulesMap = collections.MergeMaps(modulesMap, dependencies)
		}
	}

	return modulesMap, nil
}

// Create a TerraformModule struct for the Terraform module specified by the given Terragrunt configuration file path.
// Note that this method will NOT fill in the Dependencies field of the TerraformModule struct (see the
// crosslinkDependencies method for that).
func (stack *DefaultStack) resolveTerraformModule(ctx context.Context, l log.Logger, terragruntConfigPath string, modulesMap TerraformModulesMap, howThisModuleWasFound string) (*TerraformModule, error) {
	modulePath, err := util.CanonicalPath(filepath.Dir(terragruntConfigPath), ".")
	if err != nil {
		return nil, err
	}

	if _, ok := modulesMap[modulePath]; ok {
		return nil, nil
	}

	// Clone the options struct so we don't modify the original one. This is especially important as run --all operations
	// happen concurrently.
	l, opts, err := stack.terragruntOptions.CloneWithConfigPath(l, terragruntConfigPath)
	if err != nil {
		return nil, err
	}

	// We need to reset the original path for each module. Otherwise, this path will be set to wherever you ran run --all
	// from, which is not what any of the modules will want.
	opts.OriginalTerragruntConfigPath = terragruntConfigPath

	// If `childTerragruntConfig.ProcessedIncludes` contains the path `terragruntConfigPath`, then this is a parent config
	// which implies that `TerragruntConfigPath` must refer to a child configuration file, and the defined `IncludeConfig` must contain the path to the file itself
	// for the built-in functions `read_terragrunt_config()`, `path_relative_to_include()` to work correctly.
	var includeConfig *config.IncludeConfig

	if stack.childTerragruntConfig != nil && stack.childTerragruntConfig.ProcessedIncludes.ContainsPath(terragruntConfigPath) {
		includeConfig = &config.IncludeConfig{
			Path: terragruntConfigPath,
		}
		opts.TerragruntConfigPath = stack.terragruntOptions.OriginalTerragruntConfigPath
	}

	if collections.ListContainsElement(opts.ExcludeDirs, modulePath) {
		// module is excluded
		return &TerraformModule{Path: modulePath, Logger: l, TerragruntOptions: opts, FlagExcluded: true}, nil
	}

	parseCtx := config.NewParsingContext(ctx, l, opts).
		WithParseOption(stack.parserOptions).
		WithDecodeList(
			// Need for initializing the modules
			config.TerraformSource,

			// Need for parsing out the dependencies
			config.DependenciesBlock,
			config.DependencyBlock,
			config.FeatureFlagsBlock,
			config.ErrorsBlock,
		)

	// Credentials have to be acquired before the config is parsed, as the config may contain interpolation functions
	// that require credentials to be available.
	credsGetter := creds.NewGetter()
	if err := credsGetter.ObtainAndUpdateEnvIfNecessary(ctx, l, opts, externalcmd.NewProvider(l, opts)); err != nil {
		return nil, err
	}

	// We only partially parse the config, only using the pieces that we need in this section. This config will be fully
	// parsed at a later stage right before the action is run. This is to delay interpolation of functions until right
	// before we call out to terraform.

	// TODO: Remove lint suppression
	terragruntConfig, err := config.PartialParseConfigFile( //nolint:contextcheck
		parseCtx,
		l,
		terragruntConfigPath,
		includeConfig,
	)
	if err != nil {
		return nil, errors.New(ProcessingModuleError{
			UnderlyingError:       err,
			HowThisModuleWasFound: howThisModuleWasFound,
			ModulePath:            terragruntConfigPath,
		})
	}

	// Hack to persist readFiles. Need to discuss with team to see if there is a better way to handle this.
	stack.terragruntOptions.CloneReadFiles(opts.ReadFiles)

	terragruntSource, err := config.GetTerragruntSourceForModule(stack.terragruntOptions.Source, modulePath, terragruntConfig)
	if err != nil {
		return nil, err
	}

	opts.Source = terragruntSource

	_, defaultDownloadDir, err := options.DefaultWorkingAndDownloadDirs(stack.terragruntOptions.TerragruntConfigPath)
	if err != nil {
		return nil, err
	}

	// If we're using the default download directory, put it into the same folder as the Terragrunt configuration file.
	// If we're not using the default, then the user has specified a custom download directory, and we leave it as-is.
	if stack.terragruntOptions.DownloadDir == defaultDownloadDir {
		_, downloadDir, err := options.DefaultWorkingAndDownloadDirs(terragruntConfigPath)
		if err != nil {
			return nil, err
		}

		l.Debugf("Setting download directory for module %s to %s", filepath.Dir(opts.TerragruntConfigPath), downloadDir)
		opts.DownloadDir = downloadDir
	}

	// Fix for https://github.com/gruntwork-io/terragrunt/issues/208
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(terragruntConfigPath), "*.tf"))
	if err != nil {
		return nil, err
	}

	if (terragruntConfig.Terraform == nil || terragruntConfig.Terraform.Source == nil || *terragruntConfig.Terraform.Source == "") && matches == nil {
		l.Debugf("Module %s does not have an associated terraform configuration and will be skipped.", filepath.Dir(terragruntConfigPath))
		return nil, nil
	}

	return &TerraformModule{Stack: stack, Path: modulePath, Logger: l, Config: *terragruntConfig, TerragruntOptions: opts}, nil
}

// resolveDependenciesForModule looks through the dependencies of the given module and resolve the dependency paths listed in the module's config.
// If `skipExternal` is true, the func returns only dependencies that are inside of the current working directory, which means they are part of the environment the
// user is trying to run --all apply or run --all destroy. Note that this method will NOT fill in the Dependencies field of the TerraformModule struct (see the crosslinkDependencies method for that).
func (stack *DefaultStack) resolveDependenciesForModule(ctx context.Context, l log.Logger, module *TerraformModule, modulesMap TerraformModulesMap, skipExternal bool) (TerraformModulesMap, error) {
	if module.Config.Dependencies == nil || len(module.Config.Dependencies.Paths) == 0 {
		return TerraformModulesMap{}, nil
	}

	key := fmt.Sprintf("%s-%s-%v-%v", module.Path, stack.terragruntOptions.WorkingDir, skipExternal, stack.terragruntOptions.TerraformCommand)
	if value, ok := existingModules.Get(ctx, key); ok {
		return *value, nil
	}

	externalTerragruntConfigPaths := []string{}

	for _, dependency := range module.Config.Dependencies.Paths {
		dependencyPath, err := util.CanonicalPath(dependency, module.Path)
		if err != nil {
			return TerraformModulesMap{}, err
		}

		if skipExternal && !util.HasPathPrefix(dependencyPath, stack.terragruntOptions.WorkingDir) {
			continue
		}

		terragruntConfigPath := config.GetDefaultConfigPath(dependencyPath)

		if _, alreadyContainsModule := modulesMap[dependencyPath]; !alreadyContainsModule {
			externalTerragruntConfigPaths = append(externalTerragruntConfigPaths, terragruntConfigPath)
		}
	}

	howThesePathsWereFound := fmt.Sprintf("dependency of module at '%s'", module.Path)

	result, err := stack.resolveModules(ctx, l, externalTerragruntConfigPaths, howThesePathsWereFound)
	if err != nil {
		return nil, err
	}

	existingModules.Put(ctx, key, &result)

	return result, nil
}

// Look through the dependencies of the modules in the given map and resolve the "external" dependency paths listed in
// each modules config (i.e. those dependencies not in the given list of Terragrunt config canonical file paths).
// These external dependencies are outside of the current working directory, which means they may not be part of the
// environment the user is trying to run --all apply or run --all destroy. Therefore, this method also confirms whether the user wants
// to actually apply those dependencies or just assume they are already applied. Note that this method will NOT fill in
// the Dependencies field of the TerraformModule struct (see the crosslinkDependencies method for that).
func (stack *DefaultStack) resolveExternalDependenciesForModules(ctx context.Context, l log.Logger, modulesMap, modulesAlreadyProcessed TerraformModulesMap, recursionLevel int) (TerraformModulesMap, error) {
	allExternalDependencies := TerraformModulesMap{}
	modulesToSkip := modulesMap.mergeMaps(modulesAlreadyProcessed)

	// Simple protection from circular dependencies causing a Stack Overflow due to infinite recursion
	if recursionLevel > maxLevelsOfRecursion {
		return allExternalDependencies, errors.New(InfiniteRecursionError{RecursionLevel: maxLevelsOfRecursion, Modules: modulesToSkip})
	}

	sortedKeys := modulesMap.getSortedKeys()
	for _, key := range sortedKeys {
		module := modulesMap[key]

		externalDependencies, err := stack.resolveDependenciesForModule(ctx, l, module, modulesToSkip, false)
		if err != nil {
			return externalDependencies, err
		}

		l, moduleOpts, err := stack.terragruntOptions.CloneWithConfigPath(l, config.GetDefaultConfigPath(module.Path))
		if err != nil {
			return nil, err
		}

		for _, externalDependency := range externalDependencies {
			if _, alreadyFound := modulesToSkip[externalDependency.Path]; alreadyFound {
				continue
			}

			shouldApply := false
			if !stack.terragruntOptions.IgnoreExternalDependencies {
				shouldApply, err = module.confirmShouldApplyExternalDependency(ctx, l, externalDependency, moduleOpts)
				if err != nil {
					return externalDependencies, err
				}
			}

			externalDependency.AssumeAlreadyApplied = !shouldApply
			allExternalDependencies[externalDependency.Path] = externalDependency
		}
	}

	if len(allExternalDependencies) > 0 {
		recursiveDependencies, err := stack.resolveExternalDependenciesForModules(ctx, l, allExternalDependencies, modulesMap, recursionLevel+1)
		if err != nil {
			return allExternalDependencies, err
		}

		return allExternalDependencies.mergeMaps(recursiveDependencies), nil
	}

	return allExternalDependencies, nil
}

// ListStackDependentModules - build a map with each module and its dependent modules
func (stack *DefaultStack) ListStackDependentModules() map[string][]string {
	// build map of dependent modules
	// module path -> list of dependent modules
	var dependentModules = make(map[string][]string)

	// build initial mapping of dependent modules
	for _, module := range stack.modules {
		if len(module.Dependencies) != 0 {
			for _, dep := range module.Dependencies {
				dependentModules[dep.Path] = util.RemoveDuplicatesFromList(append(dependentModules[dep.Path], module.Path))
			}
		}
	}

	// Floyd–Warshall inspired approach to find dependent modules
	// merge map slices by key until no more updates are possible

	// Example:
	// Initial setup:
	// dependentModules["module1"] = ["module2", "module3"]
	// dependentModules["module2"] = ["module3"]
	// dependentModules["module3"] = ["module4"]
	// dependentModules["module4"] = ["module5"]

	// After first iteration: (module1 += module4, module2 += module4, module3 += module5)
	// dependentModules["module1"] = ["module2", "module3", "module4"]
	// dependentModules["module2"] = ["module3", "module4"]
	// dependentModules["module3"] = ["module4", "module5"]
	// dependentModules["module4"] = ["module5"]

	// After second iteration: (module1 += module5, module2 += module5)
	// dependentModules["module1"] = ["module2", "module3", "module4", "module5"]
	// dependentModules["module2"] = ["module3", "module4", "module5"]
	// dependentModules["module3"] = ["module4", "module5"]
	// dependentModules["module4"] = ["module5"]

	// Done, no more updates and in map we have all dependent modules for each module.

	for {
		noUpdates := true

		for module, dependents := range dependentModules {
			for _, dependent := range dependents {
				initialSize := len(dependentModules[module])
				// merge without duplicates
				list := util.RemoveDuplicatesFromList(append(dependentModules[module], dependentModules[dependent]...))
				list = util.RemoveElementFromList(list, module)

				dependentModules[module] = list
				if initialSize != len(dependentModules[module]) {
					noUpdates = false
				}
			}
		}

		if noUpdates {
			break
		}
	}

	return dependentModules
}

// Modules returns the Terraform modules in the stack.
func (stack *DefaultStack) Modules() TerraformModules {
	return stack.modules
}

// FindModuleByPath finds a module by its path.
func (stack *DefaultStack) FindModuleByPath(path string) *TerraformModule {
	for _, module := range stack.modules {
		if module.Path == path {
			return module
		}
	}

	return nil
}

// SetTerragruntConfig sets the child Terragrunt config for the stack.
func (stack *DefaultStack) SetTerragruntConfig(config *config.TerragruntConfig) {
	stack.childTerragruntConfig = config
}

// GetTerragruntConfig returns the child Terragrunt config for the stack.
func (stack *DefaultStack) GetTerragruntConfig() *config.TerragruntConfig {
	return stack.childTerragruntConfig
}

// SetParseOptions sets the parser options for the stack.
func (stack *DefaultStack) SetParseOptions(parserOptions []hclparse.Option) {
	stack.parserOptions = parserOptions
}

// GetParseOptions returns the parser options for the stack.
func (stack *DefaultStack) GetParseOptions() []hclparse.Option {
	return stack.parserOptions
}

// SetModules sets the Terraform modules for the stack.
func (stack *DefaultStack) SetModules(modules TerraformModules) {
	stack.modules = modules
}

// Lock locks the stack for concurrency control.
func (stack *DefaultStack) Lock() {
	stack.outputMu.Lock()
}

// Unlock unlocks the stack for concurrency control.
func (stack *DefaultStack) Unlock() {
	stack.outputMu.Unlock()
}
