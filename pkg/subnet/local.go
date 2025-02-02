// Copyright (C) 2022, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.
package subnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/ava-labs/avalanche-cli/pkg/application"
	"github.com/ava-labs/avalanche-cli/pkg/binutils"
	"github.com/ava-labs/avalanche-cli/pkg/constants"
	"github.com/ava-labs/avalanche-cli/pkg/ux"
	"github.com/ava-labs/avalanche-cli/pkg/vm"
	"github.com/ava-labs/avalanche-network-runner/client"
	"github.com/ava-labs/avalanche-network-runner/rpcpb"
	"github.com/ava-labs/avalanche-network-runner/utils"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/storage"
	"github.com/ava-labs/coreth/core"
	"github.com/ava-labs/coreth/params"
)

const (
	zipExtension       = "zip"
	WriteReadReadPerms = 0o644
)

type LocalSubnetDeployer struct {
	procChecker         binutils.ProcessChecker
	binChecker          binutils.BinaryChecker
	getClientFunc       getGRPCClientFunc
	binaryDownloader    binutils.PluginBinaryDownloader
	healthCheckInterval time.Duration
	app                 *application.Avalanche
	backendStartedHere  bool
	setDefaultSnapshot  setDefaultSnapshotFunc
}

func NewLocalSubnetDeployer(app *application.Avalanche) *LocalSubnetDeployer {
	return &LocalSubnetDeployer{
		procChecker:         binutils.NewProcessChecker(),
		binChecker:          binutils.NewBinaryChecker(),
		getClientFunc:       binutils.NewGRPCClient,
		binaryDownloader:    binutils.NewPluginBinaryDownloader(app.Log),
		healthCheckInterval: 100 * time.Millisecond,
		app:                 app,
		setDefaultSnapshot:  SetDefaultSnapshot,
	}
}

type getGRPCClientFunc func() (client.Client, error)

type setDefaultSnapshotFunc func(string, bool) error

// DeployToLocalNetwork does the heavy lifting:
// * it checks the gRPC is running, if not, it starts it
// * kicks off the actual deployment
func (d *LocalSubnetDeployer) DeployToLocalNetwork(chain string, chainGenesis string) (ids.ID, ids.ID, error) {
	if err := d.StartServer(); err != nil {
		return ids.Empty, ids.Empty, err
	}
	return d.doDeploy(chain, chainGenesis)
}

func (d *LocalSubnetDeployer) StartServer() error {
	isRunning, err := d.procChecker.IsServerProcessRunning(d.app)
	if err != nil {
		return fmt.Errorf("failed querying if server process is running: %w", err)
	}
	if !isRunning {
		d.app.Log.Debug("gRPC server is not running")
		if err := binutils.StartServerProcess(d.app); err != nil {
			return fmt.Errorf("failed starting gRPC server process: %w", err)
		}
		d.backendStartedHere = true
	}
	return nil
}

// BackendStartedHere returns true if the backend was started by this run,
// or false if it found it there already
func (d *LocalSubnetDeployer) BackendStartedHere() bool {
	return d.backendStartedHere
}

// doDeploy the actual deployment to the network runner
// steps:
// - checks if the network has been started
// - install all needed plugin binaries, for the the new VM, and the already deployed VMs
// - either starts a network from the default snapshot if not started,
//   or restarts the already available network while preserving state
// - waits completion of operation
// - get from the network an available subnet ID to be used in blockchain creation
// - deploy a new blockchain for the given VM ID, genesis, and available subnet ID
// - waits completion of operation
// - show status
func (d *LocalSubnetDeployer) doDeploy(chain string, chainGenesis string) (ids.ID, ids.ID, error) {
	avalancheGoBinPath, pluginDir, err := d.SetupLocalEnv()
	if err != nil {
		return ids.Empty, ids.Empty, err
	}

	cli, err := d.getClientFunc()
	if err != nil {
		return ids.Empty, ids.Empty, fmt.Errorf("error creating gRPC Client: %s", err)
	}
	defer cli.Close()

	exists, err := storage.FileExists(chainGenesis)
	if !exists || err != nil {
		return ids.Empty, ids.Empty, fmt.Errorf(
			"evaluated chain genesis file to be at %s but it does not seem to exist", chainGenesis)
	}

	// we need the chainID just later, but it would be ugly to fail the whole deployment
	// for a JSON unmarshalling error, so let's do it here already
	genesis, err := getGenesis(chainGenesis)
	if err != nil {
		return ids.Empty, ids.Empty, fmt.Errorf("failed to unpack chain ID from genesis: %w", err)
	}
	chainID := genesis.Config.ChainID

	runDir := d.app.GetRunDir()

	ctx := binutils.GetAsyncContext()

	// check for network and get VM info
	networkBooted := true
	clusterInfo, err := d.WaitForHealthy(ctx, cli, d.healthCheckInterval)
	if err != nil {
		// TODO: use error type not string comparison
		if strings.Contains(err.Error(), "not bootstrapped") {
			networkBooted = false
		} else {
			return ids.Empty, ids.Empty, fmt.Errorf("failed to query network health: %s", err)
		}
	}

	chainVMID, err := utils.VMID(chain)
	if err != nil {
		return ids.Empty, ids.Empty, fmt.Errorf("failed to create VM ID from %s: %w", chain, err)
	}
	d.app.Log.Debug("this VM will get ID: %s", chainVMID.String())

	if alreadyDeployed(chainVMID, clusterInfo) {
		ux.Logger.PrintToUser("Subnet %s has already been deployed", chain)
		return ids.Empty, ids.Empty, nil
	}

	if err := d.installNeededPlugins(chainVMID, clusterInfo, pluginDir); err != nil {
		return ids.Empty, ids.Empty, err
	}

	ux.Logger.PrintToUser("VMs ready.")

	if !networkBooted {
		if err := d.startNetwork(ctx, cli, avalancheGoBinPath, pluginDir, runDir); err != nil {
			return ids.Empty, ids.Empty, err
		}
	}

	clusterInfo, err = d.WaitForHealthy(ctx, cli, d.healthCheckInterval)
	if err != nil {
		return ids.Empty, ids.Empty, fmt.Errorf("failed to query network health: %s", err)
	}
	subnetIDs := clusterInfo.Subnets
	numBlockchains := len(clusterInfo.CustomVms)

	// in order to make subnet deploy faster, a set of validated subnet IDs is preloaded
	// in the bootstrap snapshot
	// we select one to be used for creating the next blockchain, for that we use the
	// number of currently created blockchains as the index to select the next subnet ID,
	// so we get incremental selection
	sort.Strings(subnetIDs)
	if len(subnetIDs) == 0 {
		return ids.Empty, ids.Empty, errors.New("the network has not preloaded subnet IDs")
	}
	subnetIDStr := subnetIDs[numBlockchains%len(subnetIDs)]

	// create a new blockchain on the already started network, associated to
	// the given VM ID, genesis, and available subnet ID
	blockchainSpecs := []*rpcpb.BlockchainSpec{
		{
			VmName:   chain,
			Genesis:  chainGenesis,
			SubnetId: &subnetIDStr,
		},
	}
	deployBlockchainsInfo, err := cli.CreateBlockchains(
		ctx,
		blockchainSpecs,
	)
	if err != nil {
		return ids.Empty, ids.Empty, fmt.Errorf("failed to deploy blockchain :%s", err)
	}

	d.app.Log.Debug(deployBlockchainsInfo.String())

	fmt.Println()
	ux.Logger.PrintToUser("Blockchain has been deployed. Wait until network acknowledges...")

	clusterInfo, err = d.WaitForHealthy(ctx, cli, d.healthCheckInterval)
	if err != nil {
		return ids.Empty, ids.Empty, fmt.Errorf("failed to query network health: %s", err)
	}

	endpoints := GetEndpoints(clusterInfo)

	fmt.Println()
	ux.Logger.PrintToUser("Network ready to use. Local network node endpoints:")
	ux.PrintTableEndpoints(clusterInfo)
	fmt.Println()

	firstURL := endpoints[0]
	tokenName := d.app.GetTokenName(chain)

	ux.Logger.PrintToUser("Metamask connection details (any node URL from above works):")
	ux.Logger.PrintToUser("RPC URL:          %s", firstURL[strings.LastIndex(firstURL, "http"):])
	for address := range genesis.Alloc {
		amount := genesis.Alloc[address].Balance
		formattedAmount := new(big.Int).Div(amount, big.NewInt(params.Ether))
		if address == vm.PrefundedEwoqAddress {
			ux.Logger.PrintToUser("Funded address:   %s with %s (10^18) - private key: %s", address, formattedAmount.String(), vm.PrefundedEwoqPrivate)
		} else {
			ux.Logger.PrintToUser("Funded address:   %s with %s", address, formattedAmount.String())
		}
	}

	ux.Logger.PrintToUser("Network name:     %s", chain)
	ux.Logger.PrintToUser("Chain ID:         %s", chainID)
	ux.Logger.PrintToUser("Currency Symbol:  %s", tokenName)

	// we can safely ignore errors here as the subnets have already been generated
	subnetID, _ := ids.FromString(subnetIDStr)
	var blockchainID ids.ID
	for _, info := range clusterInfo.CustomVms {
		if info.VmId == chainVMID.String() {
			blockchainID, _ = ids.FromString(info.BlockchainId)
		}
	}
	return subnetID, blockchainID, nil
}

// SetupLocalEnv also does some heavy lifting:
// * sets up default snapshot if not installed
// * checks if avalanchego is installed in the local binary path
// * if not, it downloads it and installs it (os - and archive dependent)
// * returns the location of the avalanchego path and plugin
func (d *LocalSubnetDeployer) SetupLocalEnv() (string, string, error) {
	err := d.setDefaultSnapshot(d.app.GetSnapshotsDir(), false)
	if err != nil {
		return "", "", fmt.Errorf("failed setting up snapshots: %w", err)
	}

	avagoDir, err := d.setupLocalEnv()
	if err != nil {
		return "", "", fmt.Errorf("failed setting up local environment: %w", err)
	}

	pluginDir := filepath.Join(avagoDir, "plugins")
	avalancheGoBinPath := filepath.Join(avagoDir, "avalanchego")

	exists, err := storage.FolderExists(pluginDir)
	if !exists || err != nil {
		return "", "", fmt.Errorf("evaluated pluginDir to be %s but it does not exist", pluginDir)
	}

	// TODO: we need some better version management here
	// * compare latest to local version
	// * decide if force update or give user choice
	exists, err = storage.FileExists(avalancheGoBinPath)
	if !exists || err != nil {
		return "", "", fmt.Errorf("evaluated avalancheGoBinPath to be %s but it does not exist", avalancheGoBinPath)
	}

	return avalancheGoBinPath, pluginDir, nil
}

func (d *LocalSubnetDeployer) setupLocalEnv() (string, error) {
	binDir := filepath.Join(d.app.GetBaseDir(), constants.AvalancheCliBinDir)
	binPrefix := "avalanchego-v"

	exists, avagoDir, err := d.binChecker.ExistsWithLatestVersion(binDir, binPrefix)
	if err != nil {
		return "", fmt.Errorf("failed trying to locate avalanchego binary: %s", binDir)
	}
	if exists {
		d.app.Log.Debug("local avalanchego found. skipping installation")
		return avagoDir, nil
	}

	ux.Logger.PrintToUser("Installing avalanchego...")

	// TODO: we are hardcoding the release version
	// until we have a better binary, dependency and version management
	// as per https://github.com/ava-labs/avalanche-cli/pull/17#discussion_r887164924
	version := constants.AvalancheGoReleaseVersion
	/*
		version, err := binutils.GetLatestReleaseVersion(constants.LatestAvagoReleaseURL)
		if err != nil {
			return "", fmt.Errorf("failed to get latest avalanchego version: %s", err)
		}
	*/

	d.app.Log.Info("Avalanchego version is: %s", version)

	// TODO: would be nice if we could also here just use binutils.DownloadLatestReleaseVersion(),
	// but unfortunately we don't have a consistent naming scheme between avalanchego and subnet-evm
	// releases and names (and supported `goos`).
	// Doing so therefore would require adding some questionable complexity.
	// The goal MUST be to have some sort of mature binary management

	// NOTE: if any of the underlying URLs change (github changes, release file names, etc.) this fails
	arch := runtime.GOARCH
	goos := runtime.GOOS
	var avalanchegoURL string
	var ext string

	switch goos {
	case "linux":
		avalanchegoURL = fmt.Sprintf(
			"https://github.com/ava-labs/avalanchego/releases/download/%s/avalanchego-linux-%s-%s.tar.gz",
			version,
			arch,
			version,
		)
		ext = "tar.gz"
	case "darwin":
		avalanchegoURL = fmt.Sprintf(
			"https://github.com/ava-labs/avalanchego/releases/download/%s/avalanchego-macos-%s.zip",
			version,
			version,
		)
		ext = zipExtension
		// EXPERMENTAL WIN, no support
	case "windows":
		avalanchegoURL = fmt.Sprintf(
			"https://github.com/ava-labs/avalanchego/releases/download/%s/avalanchego-win-%s-experimental.zip",
			version,
			version,
		)
		ext = zipExtension
	default:
		return "", fmt.Errorf("OS not supported: %s", goos)
	}

	d.app.Log.Debug("starting download from %s...", avalanchegoURL)

	resp, err := http.Get(avalanchegoURL)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected http status code: %d", resp.StatusCode)
	}
	defer resp.Body.Close()

	archive, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	d.app.Log.Debug("download successful. installing archive...")
	if err := binutils.InstallArchive(ext, archive, binDir); err != nil {
		return "", err
	}
	avagoSubDir := "avalanchego-" + version
	if ext == zipExtension {
		// zip contains a build subdir instead of the avagoSubDir expected from tar.gz
		if err := os.Rename(filepath.Join(binDir, "build"), filepath.Join(binDir, avagoSubDir)); err != nil {
			return "", err
		}
	}
	ux.Logger.PrintToUser("Avalanchego installation successful")
	return filepath.Join(binDir, avagoSubDir), nil
}

// WaitForHealthy polls continuously until the network is ready to be used
func (d *LocalSubnetDeployer) WaitForHealthy(
	ctx context.Context,
	cli client.Client,
	healthCheckInterval time.Duration,
) (*rpcpb.ClusterInfo, error) {
	cancel := make(chan struct{})
	defer close(cancel)
	go ux.PrintWait(cancel)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(healthCheckInterval):
			d.app.Log.Debug("polling for health...")
			resp, err := cli.Health(ctx)
			if err != nil {
				return nil, fmt.Errorf("the health check failed to complete. The server might be down or have crashed, check the logs! %s", err)
			}
			if resp.ClusterInfo == nil {
				d.app.Log.Debug("warning: ClusterInfo is nil. trying again...")
				continue
			}
			if !resp.ClusterInfo.Healthy {
				d.app.Log.Debug("network is not healthy. polling again...")
				continue
			}
			if !resp.ClusterInfo.CustomVmsHealthy {
				d.app.Log.Debug("network is up but custom VMs are not healthy. polling again...")
				continue
			}
			d.app.Log.Debug("network is up and custom VMs are up")
			return resp.ClusterInfo, nil
		}
	}
}

// GetEndpoints get a human readable list of endpoints from clusterinfo
func GetEndpoints(clusterInfo *rpcpb.ClusterInfo) []string {
	endpoints := []string{}
	for _, nodeInfo := range clusterInfo.NodeInfos {
		for blockchainID, vmInfo := range clusterInfo.CustomVms {
			endpoints = append(endpoints, fmt.Sprintf("Endpoint at node %s for blockchain %q with VM ID %q: %s/ext/bc/%s/rpc", nodeInfo.Name, blockchainID, vmInfo.VmId, nodeInfo.GetUri(), blockchainID))
		}
	}
	return endpoints
}

// return true if vm has already been deployed
func alreadyDeployed(chainVMID ids.ID, clusterInfo *rpcpb.ClusterInfo) bool {
	if clusterInfo != nil {
		for _, vmInfo := range clusterInfo.CustomVms {
			if vmInfo.VmId == chainVMID.String() {
				return true
			}
		}
	}
	return false
}

// get list of all needed plugins and install them
func (d *LocalSubnetDeployer) installNeededPlugins(chainVMID ids.ID, clusterInfo *rpcpb.ClusterInfo, pluginDir string) error {
	toInstallVMIDs := map[string]struct{}{}
	toInstallVMIDs[chainVMID.String()] = struct{}{}
	if clusterInfo != nil {
		for _, vmInfo := range clusterInfo.CustomVms {
			toInstallVMIDs[vmInfo.VmId] = struct{}{}
		}
	}
	binDir := filepath.Join(d.app.GetBaseDir(), constants.AvalancheCliBinDir)
	if err := d.binaryDownloader.Download(toInstallVMIDs, pluginDir, binDir); err != nil {
		return err
	}
	return nil
}

// getGenesis extracts the chain genesis from the provided genesis file
// we don't need to check the existence of the file as we already did before
// TODO: We should probably store this in some global object when asking the user so we don't need
// to unpack this here anymore. The sidecar seems the best candidate
func getGenesis(genesisFile string) (core.Genesis, error) {
	var genesis core.Genesis
	genBytes, err := os.ReadFile(genesisFile)
	if err != nil {
		return genesis, err
	}
	if err := json.Unmarshal(genBytes, &genesis); err != nil {
		return genesis, err
	}
	return genesis, nil
}

// Initialize default snapshot with bootstrap snapshot archive
// If force flag is set to true, overwrite the default snapshot if it exists
func SetDefaultSnapshot(snapshotsDir string, force bool) error {
	bootstrapSnapshotArchivePath := filepath.Join(snapshotsDir, constants.BootstrapSnapshotArchiveName)
	if _, err := os.Stat(bootstrapSnapshotArchivePath); os.IsNotExist(err) {
		resp, err := http.Get(constants.BootstrapSnapshotURL)
		if err != nil {
			return fmt.Errorf("failed downloading bootstrap snapshot: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("failed downloading bootstrap snapshot: unexpected http status code: %d", resp.StatusCode)
		}
		defer resp.Body.Close()
		bootstrapSnapshotBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed downloading bootstrap snapshot: %w", err)
		}
		if err := os.WriteFile(bootstrapSnapshotArchivePath, bootstrapSnapshotBytes, WriteReadReadPerms); err != nil {
			return fmt.Errorf("failed writing down bootstrap snapshot: %w", err)
		}
	}
	defaultSnapshotPath := filepath.Join(snapshotsDir, "anr-snapshot-"+constants.DefaultSnapshotName)
	if force {
		os.RemoveAll(defaultSnapshotPath)
	}
	if _, err := os.Stat(defaultSnapshotPath); os.IsNotExist(err) {
		bootstrapSnapshotBytes, err := os.ReadFile(bootstrapSnapshotArchivePath)
		if err != nil {
			return fmt.Errorf("failed reading bootstrap snapshot: %w", err)
		}
		if err := binutils.InstallArchive("tar.gz", bootstrapSnapshotBytes, snapshotsDir); err != nil {
			return fmt.Errorf("failed installing bootstrap snapshot: %w", err)
		}
	}
	return nil
}

// start the network
func (d *LocalSubnetDeployer) startNetwork(
	ctx context.Context,
	cli client.Client,
	avalancheGoBinPath string,
	pluginDir string,
	runDir string,
) error {
	ux.Logger.PrintToUser("Starting network...")
	loadSnapshotOpts := []client.OpOption{
		client.WithPluginDir(pluginDir),
		client.WithExecPath(avalancheGoBinPath),
		client.WithRootDataDir(runDir),
	}

	// load global node configs if they exist
	configStr, err := d.app.Conf.LoadNodeConfig()
	if err != nil {
		return err
	}
	if configStr != "" {
		loadSnapshotOpts = append(loadSnapshotOpts, client.WithGlobalNodeConfig(configStr))
	}

	_, err = cli.LoadSnapshot(
		ctx,
		constants.DefaultSnapshotName,
		loadSnapshotOpts...,
	)
	if err != nil {
		return fmt.Errorf("failed to start network :%s", err)
	}
	return nil
}
