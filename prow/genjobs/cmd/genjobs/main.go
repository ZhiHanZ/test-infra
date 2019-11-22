/*
Copyright 2019 Istio Authors

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

package genjobs

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	flag "github.com/spf13/pflag"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	prowjob "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/config"
	"sigs.k8s.io/yaml"

	"istio.io/test-infra/prow/genjobs/pkg/util"
)

const (
	autogenHeader     = "# THIS FILE IS AUTOGENERATED. DO NOT EDIT. See genjobs/README.md\n"
	filenameSeparator = "."
	jobnameSeparator  = "_"
	gitHost           = "github.com"
	maxLabelLen       = 63
	defaultModifier   = "private"
	defaultCluster    = "default"
	yamlExt           = ".(yml|yaml)$"
)

// options are the available command-line flags.
type options struct {
	bucket           string
	cluster          string
	channel          string
	sshKeySecret     string
	modifier         string
	input            string
	output           string
	branches         []string
	presets          []string
	selector         map[string]string
	labels           map[string]string
	env              map[string]string
	orgMap           map[string]string
	jobWhitelist     sets.String
	jobBlacklist     sets.String
	repoWhitelist    sets.String
	repoBlacklist    sets.String
	jobType          sets.String
	clean            bool
	dryRun           bool
	extraRefs        bool
	resolve          bool
	sshClone         bool
	overrideSelector bool
}

// parseFlags parses the command-line flags.
func (o *options) parseFlags() {
	var (
		_jobWhitelist  []string
		_jobBlacklist  []string
		_repoWhitelist []string
		_repoBlacklist []string
		_jobType       []string
	)

	flag.StringVar(&o.bucket, "bucket", "", "GCS bucket name to upload logs and build artifacts to.")
	flag.StringVar(&o.cluster, "cluster", "", "GCP cluster to run the job(s) in.")
	flag.StringVar(&o.channel, "channel", "", "Slack channel to report job status notifications to.")
	flag.StringVar(&o.sshKeySecret, "ssh-key-secret", "", "GKE cluster secrets containing the Github ssh private key.")
	flag.StringVar(&o.modifier, "modifier", defaultModifier, "Modifier to apply to generated file and job name(s).")
	flag.StringVarP(&o.input, "input", "i", ".", "Input file or directory containing job(s) to convert.")
	flag.StringVarP(&o.output, "output", "o", ".", "Output file or directory to write generated job(s).")
	flag.StringSliceVar(&o.branches, "branches", []string{}, "Branch(es) to generate job(s) for.")
	flag.StringSliceVarP(&o.presets, "presets", "p", []string{}, "Path to file(s) containing additional presets.")
	flag.StringToStringVar(&o.selector, "selector", map[string]string{}, "Node selector(s) to constrain job(s).")
	flag.StringToStringVarP(&o.labels, "labels", "l", map[string]string{}, "Prow labels to apply to the job(s).")
	flag.StringToStringVarP(&o.env, "env", "e", map[string]string{}, "Environment variables to set for the job(s).")
	flag.StringToStringVarP(&o.orgMap, "mapping", "m", map[string]string{}, "Mapping between public and private Github organization(s).")
	flag.StringSliceVar(&_jobWhitelist, "job-whitelist", []string{}, "Job(s) to whitelist in generation process.")
	flag.StringSliceVar(&_jobBlacklist, "job-blacklist", []string{}, "Job(s) to blacklist in generation process.")
	flag.StringSliceVarP(&_repoWhitelist, "repo-whitelist", "w", []string{}, "Repositories to whitelist in generation process.")
	flag.StringSliceVarP(&_repoBlacklist, "repo-blacklist", "b", []string{}, "Repositories to blacklist in generation process.")
	flag.StringSliceVarP(&_jobType, "job-type", "t", []string{"presubmit", "postsubmit", "periodic"},
		"Job type(s) to process (e.g. presubmit, postsubmit. periodic).")
	flag.BoolVar(&o.clean, "clean", false, "Clean output directory before job(s) generation.")
	flag.BoolVar(&o.dryRun, "dry-run", false, "Run in dry run mode.")
	flag.BoolVar(&o.extraRefs, "extra-refs", false, "Apply translation to all extra refs regardless of mapping.")
	flag.BoolVar(&o.resolve, "resolve", false, "Resolve and expand values for presets in generated job(s).")
	flag.BoolVar(&o.sshClone, "ssh-clone", false, "Enable a clone of the git repository over ssh.")
	flag.BoolVar(&o.overrideSelector, "override-selector", false, "The existing node selector will be overridden rather than added to.")

	flag.Parse()

	o.jobWhitelist = sets.NewString(_jobWhitelist...)
	o.jobBlacklist = sets.NewString(_jobBlacklist...)
	o.repoWhitelist = sets.NewString(_repoWhitelist...)
	o.repoBlacklist = sets.NewString(_repoBlacklist...)
	o.jobType = sets.NewString(_jobType...)
}

// validateFlags validates the command-line flags.
func (o *options) validateFlags() error {
	var err error

	if len(o.orgMap) == 0 {
		return &util.ExitError{Message: "-m, --mapping option is required.", Code: 1}
	}

	if o.input, err = filepath.Abs(o.input); err != nil {
		return &util.ExitError{Message: fmt.Sprintf("-i, --input option invalid: %v.", o.input), Code: 1}
	}

	if o.output, err = filepath.Abs(o.output); err != nil {
		return &util.ExitError{Message: fmt.Sprintf("-o, --output option invalid: %v.", o.output), Code: 1}
	}

	for i, p := range o.presets {
		if o.presets[i], err = filepath.Abs(p); !util.HasExtension(o.presets[i], yamlExt) || err != nil {
			return &util.ExitError{Message: fmt.Sprintf("-p, --preset option invalid: %v.", o.presets[i]), Code: 1}
		}
	}

	return nil
}

// validateOrgRepo validates that the org and repo for a job pass validation and should be converted.
func validateOrgRepo(o options, org string, repo string) bool {
	_, hasOrg := o.orgMap[org]

	if !hasOrg || o.repoBlacklist.Has(repo) || (len(o.repoWhitelist) > 0 && !o.repoWhitelist.Has(repo)) {
		return false
	}

	return true
}

// validateJob validates that the job passes validation and should be converted.
func validateJob(o options, name string, patterns []string, jType string) bool {
	if o.jobBlacklist.Has(name) || (len(o.jobWhitelist) > 0 && !o.jobWhitelist.Has(name)) || !isMatchBranch(o, patterns) || !o.jobType.Has(jType) {
		return false
	}

	return true
}

// isMatchBranch validates that the branch for a job passes validation and should be converted.
func isMatchBranch(o options, patterns []string) bool {
	if len(o.branches) == 0 {
		return true
	}

	for _, branch := range o.branches {
		for _, pattern := range patterns {
			if regexp.MustCompile(pattern).MatchString(branch) {
				return true
			}
		}
	}

	return false
}

// allRefs returns true if all predicate function returns true for the array of ref.
func allRefs(array []prowjob.Refs, predicate func(val prowjob.Refs, idx int) bool) bool {
	for idx, item := range array {
		if !predicate(item, idx) {
			return false
		}
	}
	return true
}

// convertOrgRepoStr translates the provided job org and repo based on the specified org mapping.
func convertOrgRepoStr(o options, s string) string {
	org, repo := util.SplitOrgRepo(s)

	valid := validateOrgRepo(o, org, repo)

	if !valid {
		return ""
	}

	return strings.Join([]string{o.orgMap[org], repo}, "/")
}

// combinePresets reads a list of paths and aggregates the presets.
func combinePresets(paths []string) []config.Preset {
	presets := []config.Preset{}

	if len(paths) == 0 {
		return presets
	}

	for _, p := range paths {
		c, err := config.ReadJobConfig(p)
		if err != nil {
			continue
		}
		presets = append(presets, c.Presets...)
	}

	return presets
}

// mergePreset merges a preset into a job Spec based on defined labels.
func mergePreset(labels map[string]string, job *config.JobBase, preset config.Preset) {
	for l, v := range preset.Labels {
		if v2, exists := labels[l]; !exists || v != v2 {
			return
		}
	}

	for _, env := range preset.Env {
	econtainer:
		for i := range job.Spec.Containers {
			for j := range job.Spec.Containers[i].Env {
				if job.Spec.Containers[i].Env[j].Name == env.Name {
					job.Spec.Containers[i].Env[j].Value = env.Value
					continue econtainer
				}
			}

			job.Spec.Containers[i].Env = append(job.Spec.Containers[i].Env, env)
		}
	}

volume:
	for _, vol := range preset.Volumes {

		for i := range job.Spec.Volumes {
			if job.Spec.Volumes[i].Name == vol.Name {
				job.Spec.Volumes[i] = vol
				continue volume
			}

		}

		job.Spec.Volumes = append(job.Spec.Volumes, vol)
	}

	for _, volm := range preset.VolumeMounts {
	vcontainer:
		for i := range job.Spec.Containers {
			for j := range job.Spec.Containers[i].VolumeMounts {
				if job.Spec.Containers[i].VolumeMounts[j].Name == volm.Name {
					job.Spec.Containers[i].VolumeMounts[j] = volm
					continue vcontainer
				}
			}

			job.Spec.Containers[i].VolumeMounts = append(job.Spec.Containers[i].VolumeMounts, volm)
		}
	}
}

// resolvePresets resolves all preset for a particular job Spec based on defined labels.
func resolvePresets(o options, labels map[string]string, job *config.JobBase, presets []config.Preset) {
	if !o.resolve {
		return
	}

	if job.Spec != nil {
		for _, preset := range presets {
			mergePreset(labels, job, preset)
		}
	}
}

// updateJobName updates the jobs Name fields based on provided inputs.
func updateJobName(o options, job *config.JobBase) {
	suffix := ""

	if o.modifier != "" {
		suffix = jobnameSeparator + o.modifier
	}

	maxNameLen := maxLabelLen - len(suffix)

	if len(job.Name) > maxNameLen {
		job.Name = job.Name[:maxNameLen]
	}

	job.Name += suffix
}

// updateUtilityConfig updates the jobs UtilityConfig fields based on provided inputs.
func updateUtilityConfig(o options, job *config.UtilityConfig) {
	if o.bucket == "" && o.sshKeySecret == "" {
		return
	}

	if job.DecorationConfig == nil {
		job.DecorationConfig = &prowjob.DecorationConfig{}
	}

	updateGCSConfiguration(o, job.DecorationConfig)
	updateSSHKeySecrets(o, job.DecorationConfig)
}

// updateGCSConfiguration updates the jobs GCSConfiguration fields based on provided inputs.
func updateGCSConfiguration(o options, job *prowjob.DecorationConfig) {
	if o.bucket == "" {
		return
	}

	if job.GCSConfiguration == nil {
		job.GCSConfiguration = &prowjob.GCSConfiguration{
			Bucket: o.bucket,
		}
	} else {
		job.GCSConfiguration.Bucket = o.bucket
	}
}

// updateSSHKeySecrets updates the jobs SSHKeySecrets fields based on provided inputs.
func updateSSHKeySecrets(o options, job *prowjob.DecorationConfig) {
	if o.sshKeySecret == "" {
		return
	}

	if job.SSHKeySecrets == nil {
		job.SSHKeySecrets = []string{o.sshKeySecret}
	} else {
		job.SSHKeySecrets = append(job.SSHKeySecrets, o.sshKeySecret)
	}
}

// updateReporterConfig updates the jobs ReporterConfig fields based on provided inputs.
func updateReporterConfig(o options, job *config.JobBase) {
	if o.channel == "" {
		return
	}

	if job.ReporterConfig == nil {
		job.ReporterConfig = &prowjob.ReporterConfig{}
	}

	job.ReporterConfig.Slack = &prowjob.SlackReporterConfig{Channel: o.channel}
}

// updateLabels updates the jobs Labels fields based on provided inputs.
func updateLabels(o options, job *config.JobBase) {
	if len(o.labels) == 0 {
		return
	}

	if job.Labels == nil {
		job.Labels = make(map[string]string)
	}

	for labelK, labelV := range o.labels {
		job.Labels[labelK] = labelV
	}
}

// updateNodeSelector updates the jobs NodeSelector fields based on provided inputs.
func updateNodeSelector(o options, job *config.JobBase) {
	if len(o.selector) == 0 {
		return
	}

	if o.overrideSelector || job.Spec.NodeSelector == nil {
		job.Spec.NodeSelector = make(map[string]string)
	}

	for selK, selV := range o.selector {
		job.Spec.NodeSelector[selK] = selV
	}
}

// updateEnvs updates the jobs Env fields based on provided inputs.
func updateEnvs(o options, job *config.JobBase) {
	if len(o.env) == 0 {
		return
	}

	envKs := util.SortedKeys(o.env)

	for _, envK := range envKs {
	container:
		for i := range job.Spec.Containers {

			for j := range job.Spec.Containers[i].Env {
				if job.Spec.Containers[i].Env[j].Name == envK {
					job.Spec.Containers[i].Env[j].Value = o.env[envK]
					continue container
				}
			}

			job.Spec.Containers[i].Env = append(job.Spec.Containers[i].Env, v1.EnvVar{Name: envK, Value: o.env[envK]})
		}
	}
}

// updateJobBase updates the jobs JobBase fields based on provided inputs to work with private repositories.
func updateJobBase(o options, job *config.JobBase, orgrepo string) {
	job.Annotations = nil

	if o.sshClone && orgrepo != "" {
		job.CloneURI = fmt.Sprintf("git@%s:%s.git", gitHost, orgrepo)
	}

	if o.cluster != "" && o.cluster != defaultCluster {
		job.Cluster = o.cluster
	}

	updateJobName(o, job)
	updateReporterConfig(o, job)
	updateLabels(o, job)
	updateNodeSelector(o, job)
	updateEnvs(o, job)
}

// updateExtraRefs updates the jobs ExtraRefs fields based on provided inputs to work with private repositories.
func updateExtraRefs(o options, refs []prowjob.Refs) {
	for i, ref := range refs {
		org, repo := ref.Org, ref.Repo

		if o.extraRefs || validateOrgRepo(o, org, repo) {
			org = o.orgMap[org]
			refs[i].Org = org
			if o.sshClone {
				refs[i].CloneURI = fmt.Sprintf("git@%s:%s/%s.git", gitHost, org, repo)
			}
		}
	}
}

// getOutPath derives the output path from the specified input directory and current path.
func getOutPath(o options, p string, in string) string {
	segments := strings.FieldsFunc(strings.TrimPrefix(p, in), func(c rune) bool { return c == '/' })

	var (
		org  string
		repo string
		file string
	)

	switch {
	case util.HasExtension(o.output, yamlExt):
		return o.output
	case len(segments) >= 3:
		org = segments[len(segments)-3]
		repo = segments[len(segments)-2]
		file = segments[len(segments)-1]
		if newOrg, ok := o.orgMap[org]; ok {
			return filepath.Join(o.output, util.GetTopLevelOrg(newOrg), repo, util.RenameFile(`^`+util.RemoveHost(org)+`\b`, file, util.RemoveHost(newOrg)))
		}
	case len(segments) == 2:
		org = segments[len(segments)-2]
		file = segments[len(segments)-1]
		if newOrg, ok := o.orgMap[org]; ok {
			return filepath.Join(o.output, util.GetTopLevelOrg(newOrg), util.RenameFile(`^`+util.RemoveHost(org)+`\b`, file, util.RemoveHost(newOrg)))
		}
	case len(segments) == 1:
		file = segments[len(segments)-1]
		if !strings.HasPrefix(file, o.modifier) {
			return filepath.Join(o.output, o.modifier+filenameSeparator+file)
		}
	case len(segments) == 0:
		file = filepath.Base(in)
		if !strings.HasPrefix(file, o.modifier) {
			return filepath.Join(o.output, o.modifier+filenameSeparator+file)
		}
	}

	return ""
}

// cleanOutFile deletes a path and any children.
func cleanOutFile(p string) {
	if err := os.RemoveAll(p); err != nil {
		util.PrintErr(fmt.Sprintf("unable to clean file %v: %v.", p, err))
	}
}

// cleanOutDir deletes all org-mapped paths and any children.
func cleanOutDir(o options, p string) {
	for _, org := range o.orgMap {
		p = filepath.Join(p, util.GetTopLevelOrg(org))
		cleanOutFile(p)
	}
}

func handleRecover() {
	if r := recover(); r != nil {
		switch t := r.(type) {
		case string:
			util.PrintErrAndExit(errors.New(t))
		case error:
			util.PrintErrAndExit(t)
		default:
			util.PrintErrAndExit(errors.New("unknown panic"))
		}
	}
}

// writeOutFile writes presubmit and postsubmit jobs definitions to the designated output path.
func writeOutFile(p string, pre map[string][]config.Presubmit, post map[string][]config.Postsubmit, per []config.Periodic) {
	if len(pre) == 0 && len(post) == 0 && len(per) == 0 {
		return
	}

	combinedPre := map[string][]config.Presubmit{}
	combinedPost := map[string][]config.Postsubmit{}
	combinedPer := []config.Periodic{}

	existingJobs, err := config.ReadJobConfig(p)
	if err == nil {
		if existingJobs.PresubmitsStatic != nil {
			combinedPre = existingJobs.PresubmitsStatic
		}
		if existingJobs.Postsubmits != nil {
			combinedPost = existingJobs.Postsubmits
		}
		if existingJobs.Periodics != nil {
			combinedPer = existingJobs.Periodics
		}
	}

	// Combine presubmits
	for orgrepo, newPre := range pre {
		if oldPre, exists := combinedPre[orgrepo]; exists {
			combinedPre[orgrepo] = append(oldPre, newPre...)
		} else {
			combinedPre[orgrepo] = newPre
		}
	}

	// Combine postsubmits
	for orgrepo, newPost := range post {
		if oldPost, exists := combinedPost[orgrepo]; exists {
			combinedPost[orgrepo] = append(oldPost, newPost...)
		} else {
			combinedPost[orgrepo] = newPost
		}
	}

	// Combine periodics
	combinedPer = append(combinedPer, per...)

	jobConfig := config.JobConfig{}

	err = jobConfig.SetPresubmits(combinedPre)
	if err != nil {
		util.PrintErr(fmt.Sprintf("unable to set presubmits for path %v: %v.", p, err))
	}

	err = jobConfig.SetPostsubmits(combinedPost)
	if err != nil {
		util.PrintErr(fmt.Sprintf("unable to set postsubmits for path %v: %v.", p, err))
	}

	jobConfig.Periodics = combinedPer

	jobConfigYaml, err := yaml.Marshal(jobConfig)
	if err != nil {
		util.PrintErr(fmt.Sprintf("unable to marshal job config output directory: %v.", err))
		return
	}

	outBytes := []byte(autogenHeader)
	outBytes = append(outBytes, jobConfigYaml...)

	dir := filepath.Dir(p)

	err = os.MkdirAll(dir, os.ModePerm)
	if err != nil {
		util.PrintErr(fmt.Sprintf("unable to create output directory %v: %v.", dir, err))
	}

	err = ioutil.WriteFile(p, outBytes, 0644)
	if err != nil {
		util.PrintErr(fmt.Sprintf("unable to write jobs to path %v: %v.", p, err))
	}
}

// main entry point.
func Main() {
	defer handleRecover()

	var o options

	o.parseFlags()

	if err := o.validateFlags(); err != nil {
		util.PrintErrAndExit(err)
	}

	if o.clean {
		cleanOutDir(o, o.output)
	}

	presets := combinePresets(o.presets)

	_ = filepath.Walk(o.input, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		absPath, _ := filepath.Abs(p)

		if !util.HasExtension(absPath, yamlExt) {
			return nil
		}

		outPath := getOutPath(o, absPath, o.input)
		if outPath == "" {
			return nil
		}
		if o.clean {
			cleanOutFile(outPath)
		}

		jobs, err := config.ReadJobConfig(absPath)
		if err != nil {
			return nil
		}

		presubmit := map[string][]config.Presubmit{}
		postsubmit := map[string][]config.Postsubmit{}
		periodic := []config.Periodic{}

		// Presubmits
		for orgrepo, pre := range jobs.PresubmitsStatic {
			orgrepo = convertOrgRepoStr(o, orgrepo)
			if orgrepo == "" {
				continue
			}

			for _, job := range pre {
				valid := validateJob(o, job.Name, job.Branches, "presubmit")
				if !valid {
					continue
				}

				updateExtraRefs(o, job.ExtraRefs)
				updateJobBase(o, &job.JobBase, orgrepo)
				updateUtilityConfig(o, &job.UtilityConfig)
				resolvePresets(o, job.Labels, &job.JobBase, append(presets, jobs.Presets...))

				presubmit[orgrepo] = append(presubmit[orgrepo], job)
			}
		}

		// Postsubmits
		for orgrepo, post := range jobs.Postsubmits {
			orgrepo = convertOrgRepoStr(o, orgrepo)
			if orgrepo == "" {
				continue
			}

			for _, job := range post {
				valid := validateJob(o, job.Name, job.Branches, "postsubmit")
				if !valid {
					continue
				}

				updateExtraRefs(o, job.ExtraRefs)
				updateJobBase(o, &job.JobBase, orgrepo)
				updateUtilityConfig(o, &job.UtilityConfig)
				resolvePresets(o, job.Labels, &job.JobBase, append(presets, jobs.Presets...))

				postsubmit[orgrepo] = append(postsubmit[orgrepo], job)
			}
		}

		// Periodic
		for _, job := range jobs.Periodics {
			if !validateJob(o, job.Name, []string{}, "periodic") {
				continue
			}

			if len(job.ExtraRefs) == 0 {
				continue
			}

			if allRefs(job.ExtraRefs, func(val prowjob.Refs, idx int) bool {
				return !validateOrgRepo(o, val.Org, val.Repo)
			}) {
				continue
			}

			updateExtraRefs(o, job.ExtraRefs)
			updateJobBase(o, &job.JobBase, "")
			updateUtilityConfig(o, &job.UtilityConfig)
			resolvePresets(o, job.Labels, &job.JobBase, append(presets, jobs.Presets...))

			periodic = append(periodic, job)
		}

		if o.dryRun {
			fmt.Printf("write %d presubmits, %d postsubmits, and %d periodics to path %s\n", len(presubmit), len(postsubmit), len(periodic), outPath)
		} else {
			writeOutFile(outPath, presubmit, postsubmit, periodic)
		}

		return nil
	})
}