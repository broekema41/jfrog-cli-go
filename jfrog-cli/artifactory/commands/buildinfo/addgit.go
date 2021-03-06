package buildinfo

import (
	"errors"
	"fmt"
	gofrogcmd "github.com/jfrog/gofrog/io"
	"github.com/jfrog/jfrog-cli-go/jfrog-cli/artifactory/utils"
	"github.com/jfrog/jfrog-cli-go/jfrog-cli/artifactory/utils/git"
	utilsconfig "github.com/jfrog/jfrog-cli-go/jfrog-cli/utils/config"
	"github.com/jfrog/jfrog-client-go/artifactory/buildinfo"
	"github.com/jfrog/jfrog-client-go/artifactory/services"
	clientutils "github.com/jfrog/jfrog-client-go/utils"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/io/fileutils"
	"github.com/jfrog/jfrog-client-go/utils/log"
	"github.com/spf13/viper"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	GitLogLimit        = 100
	ConfigIssuesPrefix = "issues."
	ConfigParseValueError = "Failed parsing %s from configuration file: %s"
	MissingConfigurationError = "Configuration file must contain: %s"
)

func AddGit(config *BuildAddGitConfiguration) error {
	log.Info("Collecting git revision and remote url...")
	err := utils.SaveBuildGeneralDetails(config.BuildName, config.BuildNumber)
	if err != nil {
		return err
	}

	// Find .git folder if it wasn't provided in the command.
	if config.DotGitPath == "" {
		config.DotGitPath, err = fileutils.GetFileOrDirPath(".git", fileutils.Folder)
		if err != nil {
			return err
		}
	}

	// Collect URL and Revision into GitManager.
	gitManager := git.NewManager(config.DotGitPath)
	err = gitManager.ReadConfig()
	if err != nil {
		return err
	}

	// Collect issues if required.
	var issues []buildinfo.AffectedIssue
	if config.ConfigFilePath != "" {
		issues, err = config.collectBuildIssues()
		if err != nil {
			return err
		}
	}

	// Populate partials with VCS info.
	populateFunc := func(partial *buildinfo.Partial) {
		partial.Vcs = &buildinfo.Vcs{
			Url:      gitManager.GetUrl(),
			Revision: gitManager.GetRevision(),
		}

		if config.ConfigFilePath != "" {
			partial.Issues = &buildinfo.Issues{
				Tracker:                &buildinfo.Tracker{Name: config.IssuesConfig.TrackerName, Version: ""},
				AggregateBuildIssues:   config.IssuesConfig.Aggregate,
				AggregationBuildStatus: config.IssuesConfig.AggregationStatus,
				AffectedIssues:         issues,
			}
		}
	}
	err = utils.SavePartialBuildInfo(config.BuildName, config.BuildNumber, populateFunc)
	if err != nil {
		return err
	}

	// Done.
	log.Info("Collected VCS details for", config.BuildName+"/"+config.BuildNumber+".")
	return nil
}

func (config *BuildAddGitConfiguration) collectBuildIssues() ([]buildinfo.AffectedIssue, error) {
	log.Info("Collecting build issues from VCS...")

	// Initialize issues-configuration.
	config.IssuesConfig = new(IssuesConfiguration)

	// Build config from file.
	err := config.IssuesConfig.populateIssuesConfigurations(config.ConfigFilePath)
	if err != nil {
		return nil, err
	}

	// Run issues collection.
	return config.doCollect(config.IssuesConfig)
}

func (config *BuildAddGitConfiguration) doCollect(issuesConfig *IssuesConfiguration) ([]buildinfo.AffectedIssue, error) {
	// Create regex pattern.
	issueRegexp, err := clientutils.GetRegExp(issuesConfig.Regexp)
	if err != nil {
		return nil, err
	}

	// Create services manager to get build-info from Artifactory.
	sm, err := utils.CreateServiceManager(issuesConfig.ArtDetails, false)
	if err != nil {
		return nil, err
	}

	// Get latest build-info from Artifactory.
	buildInfoParams := services.BuildInfoParams{BuildName: config.BuildName, BuildNumber: "LATEST"}
	buildInfo, err := sm.GetBuildInfo(buildInfoParams)
	if err != nil {
		return nil, err
	}

	// Get previous VCS Revision from BuildInfo.
	lastVcsRevision := ""
	if buildInfo.Vcs != nil {
		lastVcsRevision = buildInfo.Vcs.Revision
	}

	// Get log with limit, starting from the latest commit.
	logCmd := &LogCmd{gitPath: config.DotGitPath, logLimit: issuesConfig.LogLimit, lastVcsRevision: lastVcsRevision}
	var foundIssues []buildinfo.AffectedIssue
	protocolRegExp := gofrogcmd.CmdOutputPattern{
		RegExp: issueRegexp,
		ExecFunc: func(pattern *gofrogcmd.CmdOutputPattern) (string, error) {
			// Reached here - means no error occurred.

			// Check for out of bound results.
			if len(pattern.MatchedResults) - 1 < issuesConfig.KeyGroupIndex || len(pattern.MatchedResults) - 1 < issuesConfig.SummaryGroupIndex {
				return "", errors.New("Unexpected result while parsing issues from git log. Make sure that the regular expression used to find issues, includes two capturing groups, for the issue ID and the summary.")
			}
			// Create found Affected Issue.
			foundIssue := buildinfo.AffectedIssue{Key: pattern.MatchedResults[issuesConfig.KeyGroupIndex], Summary: pattern.MatchedResults[issuesConfig.SummaryGroupIndex], Aggregated: false}
			if issuesConfig.TrackerUrl != "" {
				foundIssue.Url = issuesConfig.TrackerUrl + pattern.MatchedResults[issuesConfig.KeyGroupIndex]
			}
			foundIssues = append(foundIssues, foundIssue)
			log.Debug("Found issue: " + pattern.MatchedResults[issuesConfig.KeyGroupIndex])
			return "", nil
		},
	}

	// Change working dir to where .git is.
	wd, err := os.Getwd()
	if errorutils.CheckError(err) != nil {
		return nil, err
	}
	defer os.Chdir(wd)
	err = os.Chdir(config.DotGitPath)
	if errorutils.CheckError(err) != nil {
		return nil, err
	}

	// Run git command.
	_, exitOk, err := gofrogcmd.RunCmdWithOutputParser(logCmd, false, &protocolRegExp)
	if errorutils.CheckError(err) != nil {
		return nil, err
	}
	if !exitOk {
		// May happen when trying to run git log for non-existing revision.
		return nil, errorutils.CheckError(errors.New("Failed executing git log command."))
	}

	// Return found issues.
	return foundIssues, nil
}

func (ic *IssuesConfiguration) populateIssuesConfigurations(configFilePath string) (err error) {
	var vConfig *viper.Viper
	vConfig, err = utils.ReadConfigFile(configFilePath, utils.YAML)
	if err != nil {
		return err
	}

	// Validate that the config contains issues.
	if !vConfig.IsSet("issues") {
		return errorutils.CheckError(errors.New(fmt.Sprintf(MissingConfigurationError, "issues")))
	}

	// Get server-id.
	if !vConfig.IsSet(ConfigIssuesPrefix + "serverID") || vConfig.GetString(ConfigIssuesPrefix + "serverID") == "" {
		return errorutils.CheckError(errors.New(fmt.Sprintf(MissingConfigurationError, ConfigIssuesPrefix + "serverID")))
	}
	ic.setArtifactoryDetailsFromConfigFile(vConfig)
	if err != nil {
		return err
	}

	// Set log limit.
	ic.LogLimit = GitLogLimit

	// Get tracker data
	if !vConfig.IsSet(ConfigIssuesPrefix + "trackerName") {
		return errorutils.CheckError(errors.New(fmt.Sprintf(MissingConfigurationError, ConfigIssuesPrefix + "trackerName")))
	}
	ic.TrackerName = vConfig.GetString(ConfigIssuesPrefix + "trackerName")

	// Get issues pattern
	if !vConfig.IsSet(ConfigIssuesPrefix + "regexp") {
		return errorutils.CheckError(errors.New(fmt.Sprintf(MissingConfigurationError, ConfigIssuesPrefix + "regexp")))
	}
	ic.Regexp = vConfig.GetString(ConfigIssuesPrefix + "regexp")

	// Get issues base url
	if vConfig.IsSet(ConfigIssuesPrefix + "trackerUrl") {
		ic.TrackerUrl = vConfig.GetString(ConfigIssuesPrefix + "trackerUrl")
	}
	if ic.TrackerUrl != "" {
		// Url should end with '/'
		if !strings.HasSuffix(ic.TrackerUrl, "/") {
			ic.TrackerUrl += "/"
		}
	}

	// Get issues key group index
	if !vConfig.IsSet(ConfigIssuesPrefix + "keyGroupIndex") {
		return errorutils.CheckError(errors.New(fmt.Sprintf(MissingConfigurationError, ConfigIssuesPrefix + "keyGroupIndex")))
	}
	ic.KeyGroupIndex, err = strconv.Atoi(vConfig.GetString(ConfigIssuesPrefix + "keyGroupIndex"))
	if err != nil {
		return errorutils.CheckError(errors.New(fmt.Sprintf(ConfigParseValueError, ConfigIssuesPrefix + "keyGroupIndex", err.Error())))
	}

	// Get issues summary group index
	if !vConfig.IsSet(ConfigIssuesPrefix + "summaryGroupIndex") {
		return errorutils.CheckError(errors.New(fmt.Sprintf(MissingConfigurationError, ConfigIssuesPrefix + "summaryGroupIndex")))
	}
	ic.SummaryGroupIndex, err = strconv.Atoi(vConfig.GetString(ConfigIssuesPrefix + "summaryGroupIndex"))
	if err != nil {
		return errorutils.CheckError(errors.New(fmt.Sprintf(ConfigParseValueError, ConfigIssuesPrefix + "summaryGroupIndex", err.Error())))
	}

	// Get aggregation aggregate
	ic.Aggregate = false
	if vConfig.IsSet(ConfigIssuesPrefix + "aggregate") {
		ic.Aggregate, err = strconv.ParseBool(vConfig.GetString(ConfigIssuesPrefix + "aggregate"))
		if err != nil {
			return errorutils.CheckError(errors.New(fmt.Sprintf(ConfigParseValueError, ConfigIssuesPrefix + "aggregate", err.Error())))
		}
	}

	// Get aggregation status
	if vConfig.IsSet(ConfigIssuesPrefix + "aggregationStatus") {
		ic.AggregationStatus = vConfig.GetString(ConfigIssuesPrefix + "aggregationStatus")
	}

	return nil
}

func (ic *IssuesConfiguration) setArtifactoryDetailsFromConfigFile(vConfig *viper.Viper) error {
	// If serverId is empty, get default config.
	serverId := vConfig.GetString("issues.serverID")
	artDetails, err := utilsconfig.GetArtifactoryConf(serverId)
	if err != nil {
		return err
	}
	ic.ArtDetails = artDetails
	return nil
}

type BuildAddGitConfiguration struct {
	BuildName      string
	BuildNumber    string
	DotGitPath     string
	ConfigFilePath string
	IssuesConfig   *IssuesConfiguration
}

type IssuesConfiguration struct {
	ArtDetails        *utilsconfig.ArtifactoryDetails
	Regexp            string
	LogLimit          int
	TrackerUrl        string
	TrackerName       string
	KeyGroupIndex     int
	SummaryGroupIndex int
	Aggregate         bool
	AggregationStatus string
}

type LogCmd struct {
	gitPath         string
	logLimit        int
	lastVcsRevision string
}

func (logCmd *LogCmd) GetCmd() *exec.Cmd {
	var cmd []string
	cmd = append(cmd, "git")
	cmd = append(cmd, "log")
	cmd = append(cmd, "--pretty=format:%s")
	cmd = append(cmd, "-"+strconv.Itoa(logCmd.logLimit))
	if logCmd.lastVcsRevision != "" {
		cmd = append(cmd, logCmd.lastVcsRevision+"..")
	}
	return exec.Command(cmd[0], cmd[1:]...)
}

func (logCmd *LogCmd) GetEnv() map[string]string {
	return map[string]string{}
}

func (logCmd *LogCmd) GetStdWriter() io.WriteCloser {
	return nil
}

func (logCmd *LogCmd) GetErrWriter() io.WriteCloser {
	return nil
}
