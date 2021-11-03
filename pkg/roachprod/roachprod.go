// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package roachprod

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/build"
	"github.com/cockroachdb/cockroach/pkg/cli/exit"
	cld "github.com/cockroachdb/cockroach/pkg/roachprod/cloud"
	"github.com/cockroachdb/cockroach/pkg/roachprod/config"
	"github.com/cockroachdb/cockroach/pkg/roachprod/install"
	"github.com/cockroachdb/cockroach/pkg/roachprod/vm"
	"github.com/cockroachdb/cockroach/pkg/roachprod/vm/aws"
	// azure registers its provider with the top-level vm package.
	_ "github.com/cockroachdb/cockroach/pkg/roachprod/vm/azure"
	"github.com/cockroachdb/cockroach/pkg/roachprod/vm/gce"
	"github.com/cockroachdb/cockroach/pkg/roachprod/vm/local"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/httputil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/errors/oserror"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// verifyClusterName ensures that the given name conforms to
// our naming pattern of "<username>-<clustername>". The
// username must match one of the vm.Provider account names
// or the --username override.
func verifyClusterName(clusterName, username string) (string, error) {
	if len(clusterName) == 0 {
		return "", fmt.Errorf("cluster name cannot be blank")
	}
	if clusterName == config.Local {
		return clusterName, nil
	}

	alphaNum, err := regexp.Compile(`^[a-zA-Z0-9\-]+$`)
	if err != nil {
		return "", err
	}
	if !alphaNum.MatchString(clusterName) {
		return "", errors.Errorf("cluster name must match %s", alphaNum.String())
	}

	// Use the vm.Provider account names, or --username.
	var accounts []string
	if len(username) > 0 {
		accounts = []string{username}
	} else {
		seenAccounts := map[string]bool{}
		active, err := vm.FindActiveAccounts()
		if err != nil {
			return "", err
		}
		for _, account := range active {
			if !seenAccounts[account] {
				seenAccounts[account] = true
				cleanAccount := vm.DNSSafeAccount(account)
				if cleanAccount != account {
					log.Infof(context.TODO(), "WARN: using `%s' as username instead of `%s'", cleanAccount, account)
				}
				accounts = append(accounts, cleanAccount)
			}
		}
	}

	// If we see <account>-<something>, accept it.
	for _, account := range accounts {
		if strings.HasPrefix(clusterName, account+"-") && len(clusterName) > len(account)+1 {
			return clusterName, nil
		}
	}

	// Try to pick out a reasonable cluster name from the input.
	i := strings.Index(clusterName, "-")
	suffix := clusterName
	if i != -1 {
		// The user specified a username prefix, but it didn't match an active
		// account name. For example, assuming the account is "peter", `roachprod
		// create joe-perf` should be specified as `roachprod create joe-perf -u
		// joe`.
		suffix = clusterName[i+1:]
	} else {
		// The user didn't specify a username prefix. For example, assuming the
		// account is "peter", `roachprod create perf` should be specified as
		// `roachprod create peter-perf`.
		_ = 0
	}

	// Suggest acceptable cluster names.
	var suggestions []string
	for _, account := range accounts {
		suggestions = append(suggestions, fmt.Sprintf("%s-%s", account, suffix))
	}
	return "", fmt.Errorf("malformed cluster name %s, did you mean one of %s",
		clusterName, suggestions)
}

// DefaultSyncedCluster returns install.SyncedCluster with default values.
func DefaultSyncedCluster() install.SyncedCluster {
	return install.SyncedCluster{
		Name:        "",
		Tag:         "",
		CertsDir:    "./certs",
		Secure:      false,
		Quiet:       false,
		UseTreeDist: true,
		Args:        nil,
		Env: []string{
			"COCKROACH_ENABLE_RPC_COMPRESSION=false",
			"COCKROACH_UI_RELEASE_NOTES_SIGNUP_DISMISSED=true",
		},
		NumRacks:       0,
		MaxConcurrency: 32,
	}
}

var _ = DefaultSyncedCluster()

func sortedClusters() []string {
	var r []string
	for n := range install.Clusters {
		r = append(r, n)
	}
	sort.Strings(r)
	return r
}

func newCluster(opts install.SyncedCluster) (*install.SyncedCluster, error) {
	nodeNames := "all"
	{
		parts := strings.Split(opts.Name, ":")
		switch len(parts) {
		case 2:
			nodeNames = parts[1]
			fallthrough
		case 1:
			opts.Name = parts[0]
		case 0:
			return nil, fmt.Errorf("no cluster specified")
		default:
			return nil, fmt.Errorf("invalid cluster name: %s", opts.Name)
		}
	}

	c, ok := install.Clusters[opts.Name]
	if !ok {
		err := errors.Newf(`unknown cluster: %s`, opts.Name)
		err = errors.WithHintf(err, `
Available clusters:
  %s
`, strings.Join(sortedClusters(), "\n  "))
		err = errors.WithHint(err, `Use "roachprod sync" to update the list of available clusters.`)
		return nil, err
	}

	c.Impl = install.Cockroach{}
	c.NumRacks = opts.NumRacks
	if c.NumRacks > 0 {
		for i := range c.Localities {
			rack := fmt.Sprintf("rack=%d", i%opts.NumRacks)
			if c.Localities[i] != "" {
				rack = "," + rack
			}
			c.Localities[i] += rack
		}
	}

	nodes, err := install.ListNodes(nodeNames, len(c.VMs))
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if n > len(c.VMs) {
			return nil, fmt.Errorf("invalid node spec %s, cluster contains %d nodes",
				nodeNames, len(c.VMs))
		}
	}
	c.Nodes = nodes
	c.Secure = opts.Secure
	c.CertsDir = opts.CertsDir
	c.Env = opts.Env
	c.Args = opts.Args
	if opts.Tag != "" {
		c.Tag = "/" + opts.Tag
	}
	c.UseTreeDist = opts.UseTreeDist
	c.Quiet = opts.Quiet || !term.IsTerminal(int(os.Stdout.Fd()))
	c.MaxConcurrency = opts.MaxConcurrency
	return c, nil
}

// userClusterNameRegexp returns a regexp that matches all clusters owned by the
// current user.
func userClusterNameRegexp() (*regexp.Regexp, error) {
	// In general, we expect that users will have the same
	// account name across the services they're using,
	// but we still want to function even if this is not
	// the case.
	seenAccounts := map[string]bool{}
	accounts, err := vm.FindActiveAccounts()
	if err != nil {
		return nil, err
	}
	pattern := ""
	for _, account := range accounts {
		if !seenAccounts[account] {
			seenAccounts[account] = true
			if len(pattern) > 0 {
				pattern += "|"
			}
			pattern += fmt.Sprintf("(^%s-)", regexp.QuoteMeta(account))
		}
	}
	return regexp.Compile(pattern)
}

// Version returns version/build information.
func Version() string {
	info := build.GetInfo()
	return info.Long()
}

// CachedHosts returns a list of all roachprod clsuters from local cache.
func CachedHosts(cachedHostsCluster string) ([]string, error) {
	if err := LoadClusters(); err != nil {
		return nil, err
	}

	names := make([]string, 0, len(install.Clusters))
	for name := range install.Clusters {
		names = append(names, name)
	}
	sort.Strings(names)

	var retLines []string
	for _, name := range names {
		c := install.Clusters[name]
		newLine := c.Name
		// when invokved by bash-completion, cachedHostsCluster is what the user
		// has currently typed -- if this cluster matches that, expand its hosts.
		if strings.HasPrefix(cachedHostsCluster, c.Name) {
			for i := range c.VMs {
				newLine += fmt.Sprintf(" %s:%d", c.Name, i+1)
			}
		}
		retLines = append(retLines, newLine)

	}
	return retLines, nil
}

// Sync grabs an exclusive lock on the roachprod state and then proceeds to
// read the current state from the cloud and write it out to disk. The locking
// protects both the reading and the writing in order to prevent the hazard
// caused by concurrent goroutines reading cloud state in a different order
// than writing it to disk.
func Sync(quiet bool) (*cld.Cloud, error) {
	lockFile := os.ExpandEnv("$HOME/.roachprod/LOCK")
	if !quiet {
		fmt.Println("Syncing...")
	}
	// Acquire a filesystem lock so that two concurrent synchronizations of
	// roachprod state don't clobber each other.
	f, err := os.Create(lockFile)
	if err != nil {
		return nil, errors.Wrapf(err, "creating lock file %q", lockFile)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return nil, errors.Wrap(err, "acquiring lock on %q")
	}
	defer f.Close()
	cloud, err := cld.ListCloud()
	if err != nil {
		return nil, err
	}
	if err := syncHosts(cloud); err != nil {
		return nil, err
	}

	var vms vm.List
	for _, c := range cloud.Clusters {
		vms = append(vms, c.VMs...)
	}

	// Figure out if we're going to overwrite the DNS entries. We don't want to
	// overwrite if we don't have all the VMs of interest, so we only do it if we
	// have a list of all VMs from both AWS and GCE (so if both providers have
	// been used to get the VMs and for GCP also if we listed the VMs in the
	// default project).
	refreshDNS := true

	if p := vm.Providers[gce.ProviderName]; !p.Active() {
		refreshDNS = false
	} else {
		var defaultProjectFound bool
		for _, prj := range p.(*gce.Provider).GetProjects() {
			if prj == gce.DefaultProject() {
				defaultProjectFound = true
				break
			}
		}
		if !defaultProjectFound {
			refreshDNS = false
		}
	}
	if !vm.Providers[aws.ProviderName].Active() {
		refreshDNS = false
	}
	// DNS entries are maintained in the GCE DNS registry for all vms, from all
	// clouds.
	if refreshDNS {
		if !quiet {
			fmt.Println("Refreshing DNS entries...")
		}
		if err := gce.SyncDNS(vms); err != nil {
			fmt.Fprintf(os.Stderr, "failed to update %s DNS: %v", gce.Subdomain, err)
		}
	} else {
		if !quiet {
			fmt.Println("Not refreshing DNS entries. We did not have all the VMs.")
		}
	}

	if err := vm.ProvidersSequential(vm.AllProviderNames(), func(p vm.Provider) error {
		return p.CleanSSH()
	}); err != nil {
		return nil, err
	}

	if err := vm.ProvidersSequential(vm.AllProviderNames(), func(p vm.Provider) error {
		return p.ConfigSSH()
	}); err != nil {
		return nil, err
	}

	err = os.Remove(lockFile)
	if err != nil {
		return nil, err
	}

	return cloud, nil
}

// List returns a cloud.Cloud struct of all roachprod clusters matching clusterNamePattern.
// Alternatively, the 'listMine' option can be provided to get the clusters that are owned
// by the current user.
func List(quiet, listMine bool, clusterNamePattern string) (cld.Cloud, error) {
	listPattern := regexp.MustCompile(".*")
	if clusterNamePattern == "" {
		if listMine {
			var err error
			listPattern, err = userClusterNameRegexp()
			if err != nil {
				return cld.Cloud{}, err
			}
		}
	} else {
		if listMine {
			return cld.Cloud{}, errors.New("'mine' option cannot be combined with 'pattern'")
		}
		var err error
		listPattern, err = regexp.Compile(clusterNamePattern)
		if err != nil {
			return cld.Cloud{}, errors.Wrapf(err, "could not compile regex pattern: %s", clusterNamePattern)
		}
	}

	cloud, err := Sync(quiet)
	if err != nil {
		return cld.Cloud{}, err
	}

	filteredCloud := cloud.Clone()
	for name := range cloud.Clusters {
		if !listPattern.MatchString(name) {
			delete(filteredCloud.Clusters, name)
		}
	}
	return *filteredCloud, nil
}

// Run runs a command on the nodes in a cluster.
func Run(clusterOpts install.SyncedCluster, SSHOptions string, cmdArray []string) error {
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}

	// Use "ssh" if an interactive session was requested (i.e. there is no
	// remote command to run).
	if len(cmdArray) == 0 {
		return c.SSH(strings.Split(SSHOptions, " "), cmdArray)
	}

	cmd := strings.TrimSpace(strings.Join(cmdArray, " "))
	title := cmd
	if len(title) > 30 {
		title = title[:27] + "..."
	}
	return c.Run(os.Stdout, os.Stderr, c.Nodes, title, cmd)
}

// SQL runs `cockroach sql` on a remote cluster.
func SQL(clusterOpts install.SyncedCluster, remoteCockroachBinary string, cmdArray []string) error {
	config.Binary = remoteCockroachBinary
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}
	cockroach, ok := c.Impl.(install.Cockroach)
	if !ok {
		return errors.New("sql is only valid on cockroach clusters")
	}
	return cockroach.SQL(c, cmdArray)
}

// IP gets the ip addresses of the nodes in a cluster.
func IP(clusterOpts install.SyncedCluster, external bool) ([]string, error) {
	c, err := newCluster(clusterOpts)
	if err != nil {
		return nil, err
	}

	nodes := c.ServerNodes()
	ips := make([]string, len(nodes))

	if external {
		for i := 0; i < len(nodes); i++ {
			ips[i] = c.VMs[nodes[i]-1]
		}
	} else {
		c.Parallel("", len(nodes), 0, func(i int) ([]byte, error) {
			var err error
			ips[i], err = c.GetInternalIP(nodes[i])
			return nil, err
		})
	}

	return ips, nil
}

// Status retrieves the status of nodes in a cluster.
func Status(clusterOpts install.SyncedCluster) error {
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}
	c.Status()
	return nil
}

// Stage stages release and edge binaries to the cluster.
// stageOS, stageDir, version can be "" to use default values
func Stage(
	clusterOpts install.SyncedCluster, stageOS, stageDir, applicationName, version string,
) error {
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}

	os := "linux"
	if stageOS != "" {
		os = stageOS
	} else if c.IsLocal() {
		os = runtime.GOOS
	}

	dir := "."
	if stageDir != "" {
		dir = stageDir
	}

	return install.StageApplication(c, applicationName, version, os, dir)
}

// Reset resets all VMs in a cluster.
func Reset(clusterOpts install.SyncedCluster, numNodes int, username string) error {
	if numNodes <= 0 || numNodes >= 1000 {
		// Upper limit is just for safety.
		return fmt.Errorf("number of nodes must be in [1..999]")
	}

	clusterName, err := verifyClusterName(clusterOpts.Name, username)
	if err != nil {
		return err
	}

	if clusterName == config.Local {
		return nil
	}

	cloud, err := cld.ListCloud()
	if err != nil {
		return err
	}
	c, ok := cloud.Clusters[clusterName]
	if !ok {
		return errors.New("cluster not found")
	}

	return vm.FanOut(c.VMs, func(p vm.Provider, vms vm.List) error {
		return p.Reset(vms)
	})
}

// SetupSSH sets up the keys and host keys for the vms in the cluster.
func SetupSSH(clusterOpts install.SyncedCluster, username string) error {
	clusterName, err := verifyClusterName(clusterOpts.Name, username)
	if err != nil {
		return err
	}
	cloud, err := Sync(clusterOpts.Quiet)
	if err != nil {
		return err
	}
	cloudCluster, ok := cloud.Clusters[clusterName]
	if !ok {
		return fmt.Errorf("could not find %s in list of cluster", clusterName)
	}
	cloudCluster.PrintDetails()
	// Run ssh-keygen -R serially on each new VM in case an IP address has been recycled
	for _, v := range cloudCluster.VMs {
		cmd := exec.Command("ssh-keygen", "-R", v.PublicIP)
		out, err := cmd.CombinedOutput()
		if err != nil {
			log.Infof(context.TODO(), "could not clear ssh key for hostname %s:\n%s", v.PublicIP, string(out))
		}

	}

	// Wait for the nodes in the cluster to start.
	install.Clusters = map[string]*install.SyncedCluster{}
	if err := LoadClusters(); err != nil {
		return err
	}
	installCluster, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}
	// For GCP clusters we need to use the config.OSUser even if the client
	// requested the shared user.
	for i := range installCluster.VMs {
		if cloudCluster.VMs[i].Provider == gce.ProviderName {
			installCluster.Users[i] = config.OSUser.Username
		}
	}
	if err := installCluster.Wait(); err != nil {
		return err
	}
	// Fetch public keys from gcloud to set up ssh access for all users into the
	// shared ubuntu user.
	installCluster.AuthorizedKeys, err = gce.GetUserAuthorizedKeys()
	if err != nil {
		return errors.Wrap(err, "failed to retrieve authorized keys from gcloud")
	}
	return installCluster.SetupSSH()
}

// Extend extends the lifetime of the specified cluster to prevent it from being destroyed.
func Extend(clusterOpts install.SyncedCluster, lifetime time.Duration) error {
	cloud, err := cld.ListCloud()
	if err != nil {
		return err
	}

	c, ok := cloud.Clusters[clusterOpts.Name]
	if !ok {
		return fmt.Errorf("cluster %s does not exist", clusterOpts.Name)
	}

	if err := cld.ExtendCluster(c, lifetime); err != nil {
		return err
	}

	// Reload the clusters and print details.
	cloud, err = cld.ListCloud()
	if err != nil {
		return err
	}

	c, ok = cloud.Clusters[clusterOpts.Name]
	if !ok {
		return fmt.Errorf("cluster %s does not exist", clusterOpts.Name)
	}

	c.PrintDetails()
	return nil
}

// Start starts nodes on a cluster.
func Start(clusterOpts install.SyncedCluster, startOpts install.StartOptsType) error {
	install.StartOpts = startOpts
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}
	c.Start()
	return nil
}

// Monitor monitors the status of cockroach nodes in a cluster.
func Monitor(
	clusterOpts install.SyncedCluster, monitorIgnoreEmptyNodes, monitorOneShot bool,
) error {
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}
	for msg := range c.Monitor(monitorIgnoreEmptyNodes, monitorOneShot) {
		if msg.Err != nil {
			msg.Msg += "error: " + msg.Err.Error()
		}
		thisError := errors.Newf("%d: %s", msg.Index, msg.Msg)
		if msg.Err != nil || strings.Contains(msg.Msg, "dead") {
			err = errors.CombineErrors(err, thisError)
		}
		fmt.Println(thisError.Error())
	}
	return err
}

// Stop starts nodes on a cluster.
func Stop(clusterOpts install.SyncedCluster, sig int, wait bool) error {
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}
	c.Stop(sig, wait)
	return nil
}

// Init initializes the cluster.
func Init(clusterOpts install.SyncedCluster, username string) error {
	_, err := verifyClusterName(clusterOpts.Name, username)
	if err != nil {
		return err
	}

	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}
	c.Init()
	return nil
}

// Wipe wipes the nodes in a cluster.
func Wipe(clusterOpts install.SyncedCluster, wipePreserveCerts bool) error {
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}
	c.Wipe(wipePreserveCerts)
	return nil
}

// Reformat reformats disks in a cluster to use the specified filesystem.
func Reformat(clusterOpts install.SyncedCluster, fs string) error {
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}

	var fsCmd string
	switch fs {
	case vm.Zfs:
		if err := install.Install(c, []string{vm.Zfs}); err != nil {
			return err
		}
		fsCmd = `sudo zpool create -f data1 -m /mnt/data1 /dev/sdb`
	case vm.Ext4:
		fsCmd = `sudo mkfs.ext4 -F /dev/sdb && sudo mount -o defaults /dev/sdb /mnt/data1`
	default:
		return fmt.Errorf("unknown filesystem %q", fs)
	}

	err = c.Run(os.Stdout, os.Stderr, c.Nodes, "reformatting", fmt.Sprintf(`
set -euo pipefail
if sudo zpool list -Ho name 2>/dev/null | grep ^data1$; then
sudo zpool destroy -f data1
fi
if mountpoint -q /mnt/data1; then
sudo umount -f /mnt/data1
fi
%s
sudo chmod 777 /mnt/data1
`, fsCmd))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
	}
	return nil
}

// Install installs third party software.
func Install(clusterOpts install.SyncedCluster, software []string) error {
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}
	return install.Install(c, software)
}

// Download downloads 3rd party tools, using a GCS cache if possible.
func Download(clusterOpts install.SyncedCluster, src, sha, dest string) error {
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}
	return install.Download(c, src, sha, dest)
}

// DistributeCerts distributes certificates to the nodes in a cluster.
// If the certificates already exist, no action is taken.
func DistributeCerts(clusterOpts install.SyncedCluster) error {
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}
	c.DistributeCerts()
	return nil
}

// Put copies a local file to the nodes in a cluster.
func Put(clusterOpts install.SyncedCluster, src, dest string) error {
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}
	c.Put(src, dest)
	return nil
}

// Get copies a remote file from the nodes in a cluster.
// If the file is retrieved from multiple nodes the destination
// file name will be prefixed with the node number.
func Get(clusterOpts install.SyncedCluster, src, dest string) error {
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}
	c.Get(src, dest)
	return nil
}

// PgURL generates pgurls for the nodes in a cluster.
func PgURL(clusterOpts install.SyncedCluster, external bool) error {
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}
	nodes := c.ServerNodes()
	ips := make([]string, len(nodes))

	if external {
		for i := 0; i < len(nodes); i++ {
			ips[i] = c.VMs[nodes[i]-1]
		}
	} else {
		c.Parallel("", len(nodes), 0, func(i int) ([]byte, error) {
			var err error
			ips[i], err = c.GetInternalIP(nodes[i])
			return nil, err
		})
	}

	var urls []string
	for i, ip := range ips {
		if ip == "" {
			return errors.Errorf("empty ip: %v", ips)
		}
		urls = append(urls, c.Impl.NodeURL(c, ip, c.Impl.NodePort(c, nodes[i])))
	}
	fmt.Println(strings.Join(urls, " "))
	if len(urls) != len(nodes) {
		return errors.Errorf("have nodes %v, but urls %v from ips %v", nodes, urls, ips)
	}
	return nil
}

// AdminURL generates admin UI URLs for the nodes in a cluster.
func AdminURL(
	clusterOpts install.SyncedCluster, adminurlIPs, adminurlOpen bool, adminurlPath string,
) error {
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}

	for i, node := range c.ServerNodes() {
		host := vm.Name(c.Name, node) + "." + gce.Subdomain

		// verify DNS is working / fallback to IPs if not.
		if i == 0 && !adminurlIPs {
			if _, err := net.LookupHost(host); err != nil {
				fmt.Fprintf(os.Stderr, "no valid DNS (yet?). might need to re-run `sync`?\n")
				adminurlIPs = true
			}
		}

		if adminurlIPs {
			host = c.VMs[node-1]
		}
		port := install.GetAdminUIPort(c.Impl.NodePort(c, node))
		scheme := "http"
		if c.Secure {
			scheme = "https"
		}
		if !strings.HasPrefix(adminurlPath, "/") {
			adminurlPath = "/" + adminurlPath
		}
		url := fmt.Sprintf("%s://%s:%d%s", scheme, host, port, adminurlPath)
		if adminurlOpen {
			if err := exec.Command("python", "-m", "webbrowser", url).Run(); err != nil {
				return err
			}
		} else {
			fmt.Println(url)
		}
	}
	return nil
}

// Pprof TODO
func Pprof(
	clusterOpts install.SyncedCluster, duration time.Duration, heap, open bool, startingPort int,
) error {
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}

	var profType string
	var description string
	if heap {
		description = "capturing heap profile"
		profType = "heap"
	} else {
		description = "capturing CPU profile"
		profType = "profile"
	}

	outputFiles := []string{}
	mu := &syncutil.Mutex{}
	pprofPath := fmt.Sprintf("debug/pprof/%s?seconds=%d", profType, int(duration.Seconds()))

	minTimeout := 30 * time.Second
	timeout := 2 * duration
	if timeout < minTimeout {
		timeout = minTimeout
	}

	httpClient := httputil.NewClientWithTimeout(timeout)
	startTime := timeutil.Now().Unix()
	failed, err := c.ParallelE(description, len(c.ServerNodes()), 0, func(i int) ([]byte, error) {
		host := c.VMs[i]
		port := install.GetAdminUIPort(c.Impl.NodePort(c, i))
		scheme := "http"
		if c.Secure {
			scheme = "https"
		}
		outputFile := fmt.Sprintf("pprof-%s-%d-%s-%04d.out", profType, startTime, c.Name, i+1)
		outputDir := filepath.Dir(outputFile)
		file, err := ioutil.TempFile(outputDir, ".pprof")
		if err != nil {
			return nil, errors.Wrap(err, "create tmpfile for pprof download")
		}

		defer func() {
			err := file.Close()
			if err != nil && !errors.Is(err, oserror.ErrClosed) {
				fmt.Fprintf(os.Stderr, "warning: could not close temporary file")
			}
			err = os.Remove(file.Name())
			if err != nil && !oserror.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "warning: could not remove temporary file")
			}
		}()

		pprofURL := fmt.Sprintf("%s://%s:%d/%s", scheme, host, port, pprofPath)
		resp, err := httpClient.Get(context.Background(), pprofURL)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, errors.Newf("unexpected status from pprof endpoint: %s", resp.Status)
		}

		if _, err := io.Copy(file, resp.Body); err != nil {
			return nil, err
		}
		if err := file.Sync(); err != nil {
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
		if err := os.Rename(file.Name(), outputFile); err != nil {
			return nil, err
		}

		mu.Lock()
		outputFiles = append(outputFiles, outputFile)
		mu.Unlock()
		return nil, nil
	})

	for _, s := range outputFiles {
		fmt.Printf("Created %s\n", s)
	}

	if err != nil {
		sort.Slice(failed, func(i, j int) bool { return failed[i].Index < failed[j].Index })
		for _, f := range failed {
			fmt.Fprintf(os.Stderr, "%d: %+v: %s\n", f.Index, f.Err, f.Out)
		}
		exit.WithCode(exit.UnspecifiedError())
	}

	if open {
		waitCommands := []*exec.Cmd{}
		for i, file := range outputFiles {
			port := startingPort + i
			cmd := exec.Command("go", "tool", "pprof",
				"-http", fmt.Sprintf(":%d", port),
				file)
			waitCommands = append(waitCommands, cmd)
			if err := cmd.Start(); err != nil {
				return err
			}
		}

		for _, cmd := range waitCommands {
			err := cmd.Wait()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Destroy TODO
func Destroy(clusters []install.SyncedCluster, destroyAllMine bool, username string) error {
	type cloudAndName struct {
		name  string
		cloud *cld.Cloud
	}
	var cns []cloudAndName
	switch len(clusters) {
	case 0:
		if !destroyAllMine {
			return errors.New("no cluster name provided")
		}

		destroyPattern, err := userClusterNameRegexp()
		if err != nil {
			return err
		}

		cloud, err := cld.ListCloud()
		if err != nil {
			return err
		}

		for name := range cloud.Clusters {
			if destroyPattern.MatchString(name) {
				cns = append(cns, cloudAndName{name: name, cloud: cloud})
			}
		}

	default:
		if destroyAllMine {
			return errors.New("--all-mine cannot be combined with cluster names")
		}

		var cloud *cld.Cloud
		for _, cluster := range clusters {
			clusterName, err := verifyClusterName(cluster.Name, username)
			if err != nil {
				return err
			}

			if clusterName != config.Local {
				if cloud == nil {
					cloud, err = cld.ListCloud()
					if err != nil {
						return err
					}
				}

				cns = append(cns, cloudAndName{name: clusterName, cloud: cloud})
			} else {
				if err := destroyLocalCluster(cluster); err != nil {
					return err
				}
			}
		}
	}

	if err := ctxgroup.GroupWorkers(context.TODO(), len(cns), func(ctx context.Context, idx int) error {
		return destroyCluster(cns[idx].cloud, cns[idx].name)
	}); err != nil {
		return err
	}
	fmt.Println("OK")
	return nil
}

func destroyCluster(cloud *cld.Cloud, clusterName string) error {
	c, ok := cloud.Clusters[clusterName]
	if !ok {
		return fmt.Errorf("cluster %s does not exist", clusterName)
	}
	fmt.Printf("Destroying cluster %s with %d nodes\n", clusterName, len(c.VMs))
	return cld.DestroyCluster(c)
}

func destroyLocalCluster(clusterOpts install.SyncedCluster) error {
	if _, ok := install.Clusters[config.Local]; !ok {
		return fmt.Errorf("cluster %s does not exist", config.Local)
	}
	clusterOpts.Name = config.Local
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}
	c.Wipe(false)
	for _, i := range c.Nodes {
		err := os.RemoveAll(fmt.Sprintf(os.ExpandEnv("${HOME}/local/%d"), i))
		if err != nil {
			return err
		}
	}
	return os.Remove(filepath.Join(os.ExpandEnv(config.DefaultHostDir), c.Name))
}

type clusterAlreadyExistsError struct {
	name string
}

func (e *clusterAlreadyExistsError) Error() string {
	return fmt.Sprintf("cluster %s already exists", e.name)
}

func newClusterAlreadyExistsError(name string) error {
	return &clusterAlreadyExistsError{name: name}
}

func cleanupFailedCreate(clusterName string) error {
	cloud, err := cld.ListCloud()
	if err != nil {
		return err
	}
	c, ok := cloud.Clusters[clusterName]
	if !ok {
		// If the cluster doesn't exist, we didn't manage to create any VMs
		// before failing. Not an error.
		return nil
	}
	return cld.DestroyCluster(c)
}

// Create TODO
func Create(
	numNodes int, username string, createVMOpts vm.CreateOpts, clusterOpts install.SyncedCluster,
) (retErr error) {
	if numNodes <= 0 || numNodes >= 1000 {
		// Upper limit is just for safety.
		return fmt.Errorf("number of nodes must be in [1..999]")
	}

	clusterName, err := verifyClusterName(clusterOpts.Name, username)
	if err != nil {
		return err
	}
	createVMOpts.ClusterName = clusterName

	defer func() {
		if retErr == nil || clusterName == config.Local {
			return
		}
		if errors.HasType(retErr, (*clusterAlreadyExistsError)(nil)) {
			return
		}
		fmt.Fprintf(os.Stderr, "Cleaning up partially-created cluster (prev err: %s)\n", retErr)
		if err := cleanupFailedCreate(clusterName); err != nil {
			fmt.Fprintf(os.Stderr, "Error while cleaning up partially-created cluster: %s\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "Cleaning up OK\n")
		}
	}()

	if clusterName != config.Local {
		cloud, err := cld.ListCloud()
		if err != nil {
			return err
		}
		if _, ok := cloud.Clusters[clusterName]; ok {
			return newClusterAlreadyExistsError(clusterName)
		}
	} else {
		if _, ok := install.Clusters[clusterName]; ok {
			return newClusterAlreadyExistsError(clusterName)
		}

		// If the local cluster is being created, force the local Provider to be used
		createVMOpts.VMProviders = []string{local.ProviderName}
	}

	if createVMOpts.SSDOpts.FileSystem == vm.Zfs {
		for _, provider := range createVMOpts.VMProviders {
			if provider != gce.ProviderName {
				return fmt.Errorf(
					"creating a node with --filesystem=zfs is currently only supported on gce",
				)
			}
		}
	}

	fmt.Printf("Creating cluster %s with %d nodes\n", clusterName, numNodes)
	if createErr := cld.CreateCluster(numNodes, createVMOpts); createErr != nil {
		return createErr
	}

	// Just create directories for the local cluster as there's no need for ssh.
	if clusterName == config.Local {
		for i := 0; i < numNodes; i++ {
			err := os.MkdirAll(fmt.Sprintf(os.ExpandEnv("${HOME}/local/%d"), i+1), 0755)
			if err != nil {
				return err
			}
		}
		return nil
	}
	return SetupSSH(clusterOpts, username)
}

// GC garbage-collects expired clusters and unused SSH keypairs in AWS.
func GC(dryrun bool, slackToken string) error {
	config.SlackToken = slackToken
	cloud, err := cld.ListCloud()
	if err == nil {
		// GCClusters depends on ListCloud so only call it if ListCloud runs without errors
		err = cld.GCClusters(cloud, dryrun)
	}
	otherErr := cld.GCAWSKeyPairs(dryrun)
	return errors.CombineErrors(err, otherErr)
}

// LogsOpts TODO
type LogsOpts struct {
	Dir, Filter, ProgramFilter string
	Interval                   time.Duration
	From, To                   time.Time
	Out                        io.Writer
}

// Logs TODO
func Logs(logsOpts LogsOpts, clusterOpts install.SyncedCluster, dest, username string) error {
	c, err := newCluster(clusterOpts)
	if err != nil {
		return err
	}
	return c.Logs(logsOpts.Dir, dest, username, logsOpts.Filter, logsOpts.ProgramFilter, logsOpts.Interval, logsOpts.From, logsOpts.To, logsOpts.Out)
}

// StageURL TODO
func StageURL(applicationName, version, stageOS string) ([]*url.URL, error) {
	os := runtime.GOOS
	if stageOS != "" {
		os = stageOS
	}
	urls, err := install.URLsForApplication(applicationName, version, os)
	if err != nil {
		return nil, err
	}
	return urls, nil
}