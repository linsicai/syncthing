// Copyright (C) 2014 Jakob Borg and Contributors (see the CONTRIBUTORS file).
// All rights reserved. Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package main

import (
	"crypto/sha1"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"code.google.com/p/go.crypto/bcrypt"
	"github.com/juju/ratelimit"
	"github.com/syncthing/syncthing/internal/config"
	"github.com/syncthing/syncthing/internal/discover"
	"github.com/syncthing/syncthing/internal/events"
	"github.com/syncthing/syncthing/internal/files"
	"github.com/syncthing/syncthing/internal/logger"
	"github.com/syncthing/syncthing/internal/model"
	"github.com/syncthing/syncthing/internal/protocol"
	"github.com/syncthing/syncthing/internal/upgrade"
	"github.com/syncthing/syncthing/internal/upnp"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

var (
	Version     = "unknown-dev"
	BuildEnv    = "default"
	BuildStamp  = "0"
	BuildDate   time.Time
	BuildHost   = "unknown"
	BuildUser   = "unknown"
	LongVersion string
	GoArchExtra string // "", "v5", "v6", "v7"
)

const (
	exitSuccess            = 0
	exitError              = 1
	exitNoUpgradeAvailable = 2
	exitRestarting         = 3
	exitUpgrading          = 4
)

var l = logger.DefaultLogger
var innerProcess = os.Getenv("STNORESTART") != ""

func init() {
	if Version != "unknown-dev" {
		// If not a generic dev build, version string should come from git describe
		exp := regexp.MustCompile(`^v\d+\.\d+\.\d+(-[a-z0-9]+)*(\+\d+-g[0-9a-f]+)?(-dirty)?$`)
		if !exp.MatchString(Version) {
			l.Fatalf("Invalid version string %q;\n\tdoes not match regexp %v", Version, exp)
		}
	}

	stamp, _ := strconv.Atoi(BuildStamp)
	BuildDate = time.Unix(int64(stamp), 0)

	date := BuildDate.UTC().Format("2006-01-02 15:04:05 MST")
	LongVersion = fmt.Sprintf("syncthing %s (%s %s-%s %s) %s@%s %s", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH, BuildEnv, BuildUser, BuildHost, date)

	if os.Getenv("STTRACE") != "" {
		logFlags = log.Ltime | log.Ldate | log.Lmicroseconds | log.Lshortfile
	}
}

var (
	cfg            config.Configuration
	myID           protocol.DeviceID
	confDir        string
	logFlags       int = log.Ltime
	writeRateLimit *ratelimit.Bucket
	readRateLimit  *ratelimit.Bucket
	stop           = make(chan int)
	discoverer     *discover.Discoverer
	externalPort   int
	cert           tls.Certificate
)

const (
	usage      = "syncthing [options]"
	extraUsage = `The value for the -logflags option is a sum of the following:

   1  Date
   2  Time
   4  Microsecond time
   8  Long filename
  16  Short filename

I.e. to prefix each log line with date and time, set -logflags=3 (1 + 2 from
above). The value 0 is used to disable all of the above. The default is to
show time only (2).

The following enviroment variables are interpreted by syncthing:

 STGUIADDRESS  Override GUI listen address set in config. Expects protocol type
               followed by hostname or an IP address, followed by a port, such
               as "https://127.0.0.1:8888".

 STGUIAUTH     Override GUI authentication credentials set in config. Expects
               a colon separated username and password, such as "admin:secret".

 STGUIAPIKEY   Override GUI API key set in config.

 STNORESTART   Do not attempt to restart when requested to, instead just exit.
               Set this variable when running under a service manager such as
               runit, launchd, etc.

 STTRACE       A comma separated string of facilities to trace. The valid
               facility strings:
               - "beacon"   (the beacon package)
               - "discover" (the discover package)
               - "events"   (the events package)
               - "files"    (the files package)
               - "net"      (the main package; connections & network messages)
               - "model"    (the model package)
               - "scanner"  (the scanner package)
               - "stats"    (the stats package)
               - "upnp"     (the upnp package)
               - "xdr"      (the xdr package)
               - "all"      (all of the above)

 STGUIASSETS   Directory to load GUI assets from. Overrides compiled in assets.

 STPROFILER    Set to a listen address such as "127.0.0.1:9090" to start the
               profiler with HTTP access.

 STCPUPROFILE  Write a CPU profile to cpu-$pid.pprof on exit.

 STHEAPPROFILE Write heap profiles to heap-$pid-$timestamp.pprof each time
               heap usage increases.

 STPERFSTATS   Write running performance statistics to perf-$pid.csv. Not
               supported on Windows.

 GOMAXPROCS    Set the maximum number of CPU cores to use. Defaults to all
               available CPU cores.`
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

// Command line options
var (
	reset             bool
	showVersion       bool
	doUpgrade         bool
	doUpgradeCheck    bool
	noBrowser         bool
	generateDir       string
	guiAddress        string
	guiAuthentication string
	guiAPIKey         string
)

func main() {
	flag.StringVar(&confDir, "home", getDefaultConfDir(), "Set configuration directory")
	flag.BoolVar(&reset, "reset", false, "Prepare to resync from cluster")
	flag.BoolVar(&showVersion, "version", false, "Show version")
	flag.BoolVar(&doUpgrade, "upgrade", false, "Perform upgrade")
	flag.BoolVar(&doUpgradeCheck, "upgrade-check", false, "Check for available upgrade")
	flag.BoolVar(&noBrowser, "no-browser", false, "Do not start browser")
	flag.StringVar(&generateDir, "generate", "", "Generate key in specified dir")
	flag.StringVar(&guiAddress, "gui-address", "", "Override GUI address")
	flag.StringVar(&guiAuthentication, "gui-authentication", "", "Override GUI authentication. Expects 'username:password'")
	flag.StringVar(&guiAPIKey, "gui-apikey", "", "Override GUI API key")
	flag.IntVar(&logFlags, "logflags", logFlags, "Set log flags")
	flag.Usage = usageFor(flag.CommandLine, usage, extraUsage)
	flag.Parse()

	if showVersion {
		fmt.Println(LongVersion)
		return
	}

	l.SetFlags(logFlags)

	if generateDir != "" {
		dir := expandTilde(generateDir)

		info, err := os.Stat(dir)
		if err != nil {
			l.Fatalln("generate:", err)
		}
		if !info.IsDir() {
			l.Fatalln(dir, "is not a directory")
		}

		cert, err := loadCert(dir, "")
		if err == nil {
			l.Warnln("Key exists; will not overwrite.")
			l.Infoln("Device ID:", protocol.NewDeviceID(cert.Certificate[0]))
			return
		}

		newCertificate(dir, "")
		cert, err = loadCert(dir, "")
		if err != nil {
			l.Fatalln("load cert:", err)
		}
		if err == nil {
			l.Infoln("Device ID:", protocol.NewDeviceID(cert.Certificate[0]))
		}
		return
	}

	confDir = expandTilde(confDir)

	if info, err := os.Stat(confDir); err == nil && !info.IsDir() {
		l.Fatalln("Config directory", confDir, "is not a directory")
	}

	// Ensure that our home directory exists.
	ensureDir(confDir, 0700)

	if doUpgrade || doUpgradeCheck {
		rel, err := upgrade.LatestRelease(strings.Contains(Version, "-beta"))
		if err != nil {
			l.Fatalln("Upgrade:", err) // exits 1
		}

		if upgrade.CompareVersions(rel.Tag, Version) <= 0 {
			l.Infof("No upgrade available (current %q >= latest %q).", Version, rel.Tag)
			os.Exit(exitNoUpgradeAvailable)
		}

		l.Infof("Upgrade available (current %q < latest %q)", Version, rel.Tag)

		if doUpgrade {
			// Use leveldb database locks to protect against concurrent upgrades
			_, err = leveldb.OpenFile(filepath.Join(confDir, "index"), &opt.Options{CachedOpenFiles: 100})
			if err != nil {
				l.Fatalln("Cannot upgrade, database seems to be locked. Is another copy of Syncthing already running?")
			}

			err = upgrade.UpgradeTo(rel, GoArchExtra)
			if err != nil {
				l.Fatalln("Upgrade:", err) // exits 1
			}
			l.Okf("Upgraded to %q", rel.Tag)
			return
		} else {
			return
		}
	}

	if reset {
		resetFolders()
		return
	}

	if os.Getenv("STNORESTART") != "" {
		syncthingMain()
	} else {
		monitorMain()
	}
}

func syncthingMain() {
	var err error

	if len(os.Getenv("GOGC")) == 0 {
		debug.SetGCPercent(25)
	}

	if len(os.Getenv("GOMAXPROCS")) == 0 {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}

	events.Default.Log(events.Starting, map[string]string{"home": confDir})

	if _, err = os.Stat(confDir); err != nil && confDir == getDefaultConfDir() {
		// We are supposed to use the default configuration directory. It
		// doesn't exist. In the past our default has been ~/.syncthing, so if
		// that directory exists we move it to the new default location and
		// continue. We don't much care if this fails at this point, we will
		// be checking that later.

		var oldDefault string
		if runtime.GOOS == "windows" {
			oldDefault = filepath.Join(os.Getenv("AppData"), "Syncthing")
		} else {
			oldDefault = expandTilde("~/.syncthing")
		}
		if _, err := os.Stat(oldDefault); err == nil {
			os.MkdirAll(filepath.Dir(confDir), 0700)
			if err := os.Rename(oldDefault, confDir); err == nil {
				l.Infoln("Moved config dir", oldDefault, "to", confDir)
			}
		}
	}

	// Ensure that that we have a certificate and key.
	cert, err = loadCert(confDir, "")
	if err != nil {
		newCertificate(confDir, "")
		cert, err = loadCert(confDir, "")
		if err != nil {
			l.Fatalln("load cert:", err)
		}
	}

	myID = protocol.NewDeviceID(cert.Certificate[0])
	l.SetPrefix(fmt.Sprintf("[%s] ", myID.String()[:5]))

	l.Infoln(LongVersion)
	l.Infoln("My ID:", myID)

	// Prepare to be able to save configuration

	cfgFile := filepath.Join(confDir, "config.xml")

	var myName string

	// Load the configuration file, if it exists.
	// If it does not, create a template.

	cfg, err = config.Load(cfgFile, myID)
	if err == nil {
		myCfg := cfg.GetDeviceConfiguration(myID)
		if myCfg == nil || myCfg.Name == "" {
			myName, _ = os.Hostname()
		} else {
			myName = myCfg.Name
		}
	} else {
		l.Infoln("No config file; starting with empty defaults")
		myName, _ = os.Hostname()
		defaultFolder := filepath.Join(getHomeDir(), "Sync")

		cfg = config.New(cfgFile, myID)
		cfg.Folders = []config.FolderConfiguration{
			{
				ID:              "default",
				Path:            defaultFolder,
				RescanIntervalS: 60,
				Devices:         []config.FolderDeviceConfiguration{{DeviceID: myID}},
			},
		}
		cfg.Devices = []config.DeviceConfiguration{
			{
				DeviceID:  myID,
				Addresses: []string{"dynamic"},
				Name:      myName,
			},
		}

		port, err := getFreePort("127.0.0.1", 8080)
		if err != nil {
			l.Fatalln("get free port (GUI):", err)
		}
		cfg.GUI.Address = fmt.Sprintf("127.0.0.1:%d", port)

		port, err = getFreePort("0.0.0.0", 22000)
		if err != nil {
			l.Fatalln("get free port (BEP):", err)
		}
		cfg.Options.ListenAddress = []string{fmt.Sprintf("0.0.0.0:%d", port)}

		cfg.Save()
		l.Infof("Edit %s to taste or use the GUI\n", cfgFile)
	}

	if profiler := os.Getenv("STPROFILER"); len(profiler) > 0 {
		go func() {
			l.Debugln("Starting profiler on", profiler)
			runtime.SetBlockProfileRate(1)
			err := http.ListenAndServe(profiler, nil)
			if err != nil {
				l.Fatalln(err)
			}
		}()
	}

	// The TLS configuration is used for both the listening socket and outgoing
	// connections.

	tlsCfg := &tls.Config{
		Certificates:           []tls.Certificate{cert},
		NextProtos:             []string{"bep/1.0"},
		ServerName:             myID.String(),
		ClientAuth:             tls.RequestClientCert,
		SessionTicketsDisabled: true,
		InsecureSkipVerify:     true,
		MinVersion:             tls.VersionTLS12,
	}

	// If the read or write rate should be limited, set up a rate limiter for it.
	// This will be used on connections created in the connect and listen routines.

	if cfg.Options.MaxSendKbps > 0 {
		writeRateLimit = ratelimit.NewBucketWithRate(float64(1000*cfg.Options.MaxSendKbps), int64(5*1000*cfg.Options.MaxSendKbps))
	}
	if cfg.Options.MaxRecvKbps > 0 {
		readRateLimit = ratelimit.NewBucketWithRate(float64(1000*cfg.Options.MaxRecvKbps), int64(5*1000*cfg.Options.MaxRecvKbps))
	}

	// If this is the first time the user runs v0.9, archive the old indexes and config.
	archiveLegacyConfig()

	db, err := leveldb.OpenFile(filepath.Join(confDir, "index"), &opt.Options{CachedOpenFiles: 100})
	if err != nil {
		l.Fatalln("Cannot open database:", err, "- Is another copy of Syncthing already running?")
	}

	// Remove database entries for folders that no longer exist in the config
	folderMap := cfg.FolderMap()
	for _, folder := range files.ListFolders(db) {
		if _, ok := folderMap[folder]; !ok {
			l.Infof("Cleaning data for dropped folder %q", folder)
			files.DropFolder(db, folder)
		}
	}

	m := model.NewModel(confDir, &cfg, myName, "syncthing", Version, db)

nextFolder:
	for i, folder := range cfg.Folders {
		if folder.Invalid != "" {
			continue
		}
		folder.Path = expandTilde(folder.Path)
		m.AddFolder(folder)

		fi, err := os.Stat(folder.Path)
		if m.CurrentLocalVersion(folder.ID) > 0 {
			// Safety check. If the cached index contains files but the
			// folder doesn't exist, we have a problem. We would assume
			// that all files have been deleted which might not be the case,
			// so mark it as invalid instead.
			if err != nil || !fi.IsDir() {
				l.Warnf("Stopping folder %q - path does not exist, but has files in index", folder.ID)
				cfg.Folders[i].Invalid = "folder path missing"
				continue nextFolder
			}
		} else if os.IsNotExist(err) {
			// If we don't have any files in the index, and the directory
			// doesn't exist, try creating it.
			err = os.MkdirAll(folder.Path, 0700)
		}

		if err != nil {
			// If there was another error or we could not create the
			// path, the folder is invalid.
			l.Warnf("Stopping folder %q - %v", err)
			cfg.Folders[i].Invalid = err.Error()
			continue nextFolder
		}
	}

	// GUI

	guiCfg := overrideGUIConfig(cfg.GUI, guiAddress, guiAuthentication, guiAPIKey)

	if guiCfg.Enabled && guiCfg.Address != "" {
		addr, err := net.ResolveTCPAddr("tcp", guiCfg.Address)
		if err != nil {
			l.Fatalf("Cannot start GUI on %q: %v", guiCfg.Address, err)
		} else {
			var hostOpen, hostShow string
			switch {
			case addr.IP == nil:
				hostOpen = "localhost"
				hostShow = "0.0.0.0"
			case addr.IP.IsUnspecified():
				hostOpen = "localhost"
				hostShow = addr.IP.String()
			default:
				hostOpen = addr.IP.String()
				hostShow = hostOpen
			}

			var proto = "http"
			if guiCfg.UseTLS {
				proto = "https"
			}

			urlShow := fmt.Sprintf("%s://%s/", proto, net.JoinHostPort(hostShow, strconv.Itoa(addr.Port)))
			l.Infoln("Starting web GUI on", urlShow)
			err := startGUI(guiCfg, os.Getenv("STGUIASSETS"), m)
			if err != nil {
				l.Fatalln("Cannot start GUI:", err)
			}
			if !noBrowser && cfg.Options.StartBrowser && len(os.Getenv("STRESTART")) == 0 {
				urlOpen := fmt.Sprintf("%s://%s/", proto, net.JoinHostPort(hostOpen, strconv.Itoa(addr.Port)))
				openURL(urlOpen)
			}
		}
	}

	// Clear out old indexes for other devices. Otherwise we'll start up and
	// start needing a bunch of files which are nowhere to be found. This
	// needs to be changed when we correctly do persistent indexes.
	for _, folderCfg := range cfg.Folders {
		if folderCfg.Invalid != "" {
			continue
		}
		for _, device := range folderCfg.DeviceIDs() {
			if device == myID {
				continue
			}
			m.Index(device, folderCfg.ID, nil)
		}
	}

	// Walk the folder and update the local model before establishing any
	// connections to other devices.

	m.CleanFolders()
	l.Infoln("Performing initial folder scan")
	m.ScanFolders()

	// Remove all .idx* files that don't belong to an active folder.

	validIndexes := make(map[string]bool)
	for _, folder := range cfg.Folders {
		dir := expandTilde(folder.Path)
		id := fmt.Sprintf("%x", sha1.Sum([]byte(dir)))
		validIndexes[id] = true
	}

	allIndexes, err := filepath.Glob(filepath.Join(confDir, "*.idx*"))
	if err == nil {
		for _, idx := range allIndexes {
			bn := filepath.Base(idx)
			fs := strings.Split(bn, ".")
			if len(fs) > 1 {
				if _, ok := validIndexes[fs[0]]; !ok {
					l.Infoln("Removing old index", bn)
					os.Remove(idx)
				}
			}
		}
	}

	// The default port we announce, possibly modified by setupUPnP next.

	addr, err := net.ResolveTCPAddr("tcp", cfg.Options.ListenAddress[0])
	if err != nil {
		l.Fatalln("Bad listen address:", err)
	}
	externalPort = addr.Port

	// UPnP

	if cfg.Options.UPnPEnabled {
		setupUPnP()
	}

	// Routine to connect out to configured devices
	discoverer = discovery(externalPort)
	go listenConnect(myID, m, tlsCfg)

	for _, folder := range cfg.Folders {
		if folder.Invalid != "" {
			continue
		}

		// Routine to pull blocks from other devices to synchronize the local
		// folder. Does not run when we are in read only (publish only) mode.
		if folder.ReadOnly {
			l.Okf("Ready to synchronize %s (read only; no external updates accepted)", folder.ID)
			m.StartFolderRO(folder.ID)
		} else {
			l.Okf("Ready to synchronize %s (read-write)", folder.ID)
			m.StartFolderRW(folder.ID)
		}
	}

	if cpuprof := os.Getenv("STCPUPROFILE"); len(cpuprof) > 0 {
		f, err := os.Create(fmt.Sprintf("cpu-%d.pprof", os.Getpid()))
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	for _, device := range cfg.Devices {
		if len(device.Name) > 0 {
			l.Infof("Device %s is %q at %v", device.DeviceID, device.Name, device.Addresses)
		}
	}

	if cfg.Options.URAccepted > 0 && cfg.Options.URAccepted < usageReportVersion {
		l.Infoln("Anonymous usage report has changed; revoking acceptance")
		cfg.Options.URAccepted = 0
	}
	if cfg.Options.URAccepted >= usageReportVersion {
		go usageReportingLoop(m)
		go func() {
			time.Sleep(10 * time.Minute)
			err := sendUsageReport(m)
			if err != nil {
				l.Infoln("Usage report:", err)
			}
		}()
	}

	if cfg.Options.RestartOnWakeup {
		go standbyMonitor()
	}

	if cfg.Options.AutoUpgradeIntervalH > 0 {
		go autoUpgrade()
	}

	events.Default.Log(events.StartupComplete, nil)
	go generateEvents()

	code := <-stop

	l.Okln("Exiting")
	os.Exit(code)
}

func generateEvents() {
	for {
		time.Sleep(300 * time.Second)
		events.Default.Log(events.Ping, nil)
	}
}

func setupUPnP() {
	if len(cfg.Options.ListenAddress) == 1 {
		_, portStr, err := net.SplitHostPort(cfg.Options.ListenAddress[0])
		if err != nil {
			l.Warnln("Bad listen address:", err)
		} else {
			// Set up incoming port forwarding, if necessary and possible
			port, _ := strconv.Atoi(portStr)
			igd, err := upnp.Discover()
			if err == nil {
				externalPort = setupExternalPort(igd, port)
				if externalPort == 0 {
					l.Warnln("Failed to create UPnP port mapping")
				} else {
					l.Infoln("Created UPnP port mapping - external port", externalPort)
				}
			} else {
				l.Infof("No UPnP gateway detected")
				if debugNet {
					l.Debugf("UPnP: %v", err)
				}
			}
			if cfg.Options.UPnPRenewal > 0 {
				go renewUPnP(port)
			}
		}
	} else {
		l.Warnln("Multiple listening addresses; not attempting UPnP port mapping")
	}
}

func setupExternalPort(igd *upnp.IGD, port int) int {
	// We seed the random number generator with the device ID to get a
	// repeatable sequence of random external ports.
	rnd := rand.NewSource(certSeed(cert.Certificate[0]))
	for i := 0; i < 10; i++ {
		r := 1024 + int(rnd.Int63()%(65535-1024))
		err := igd.AddPortMapping(upnp.TCP, r, port, "syncthing", cfg.Options.UPnPLease*60)
		if err == nil {
			return r
		}
	}
	return 0
}

func renewUPnP(port int) {
	for {
		time.Sleep(time.Duration(cfg.Options.UPnPRenewal) * time.Minute)

		igd, err := upnp.Discover()
		if err != nil {
			continue
		}

		// Just renew the same port that we already have
		if externalPort != 0 {
			err = igd.AddPortMapping(upnp.TCP, externalPort, port, "syncthing", cfg.Options.UPnPLease*60)
			if err == nil {
				l.Infoln("Renewed UPnP port mapping - external port", externalPort)
				continue
			}
		}

		// Something strange has happened. We didn't have an external port before?
		// Or perhaps the gateway has changed?
		// Retry the same port sequence from the beginning.
		r := setupExternalPort(igd, port)
		if r != 0 {
			externalPort = r
			l.Infoln("Updated UPnP port mapping - external port", externalPort)
			discoverer.StopGlobal()
			discoverer.StartGlobal(cfg.Options.GlobalAnnServer, uint16(r))
			continue
		}
		l.Warnln("Failed to update UPnP port mapping - external port", externalPort)
	}
}

func resetFolders() {
	suffix := fmt.Sprintf(".syncthing-reset-%d", time.Now().UnixNano())
	for _, folder := range cfg.Folders {
		if _, err := os.Stat(folder.Path); err == nil {
			l.Infof("Reset: Moving %s -> %s", folder.Path, folder.Path+suffix)
			os.Rename(folder.Path, folder.Path+suffix)
		}
	}

	idx := filepath.Join(confDir, "index")
	os.RemoveAll(idx)
}

func archiveLegacyConfig() {
	pat := filepath.Join(confDir, "*.idx.gz*")
	idxs, err := filepath.Glob(pat)
	if err == nil && len(idxs) > 0 {
		// There are legacy indexes. This is probably the first time we run as v0.9.
		backupDir := filepath.Join(confDir, "backup-of-v0.8")
		err = os.MkdirAll(backupDir, 0700)
		if err != nil {
			l.Warnln("Cannot archive config/indexes:", err)
			return
		}

		for _, idx := range idxs {
			l.Infof("Archiving %s", filepath.Base(idx))
			os.Rename(idx, filepath.Join(backupDir, filepath.Base(idx)))
		}

		src, err := os.Open(filepath.Join(confDir, "config.xml"))
		if err != nil {
			l.Warnf("Cannot archive config:", err)
			return
		}
		defer src.Close()

		dst, err := os.Create(filepath.Join(backupDir, "config.xml"))
		if err != nil {
			l.Warnf("Cannot archive config:", err)
			return
		}
		defer dst.Close()

		l.Infoln("Archiving config.xml")
		io.Copy(dst, src)
	}
}

func restart() {
	l.Infoln("Restarting")
	stop <- exitRestarting
}

func shutdown() {
	l.Infoln("Shutting down")
	stop <- exitSuccess
}

func listenConnect(myID protocol.DeviceID, m *model.Model, tlsCfg *tls.Config) {
	var conns = make(chan *tls.Conn)

	// Listen
	for _, addr := range cfg.Options.ListenAddress {
		go listenTLS(conns, addr, tlsCfg)
	}

	// Connect
	go dialTLS(m, conns, tlsCfg)

next:
	for conn := range conns {
		certs := conn.ConnectionState().PeerCertificates
		if cl := len(certs); cl != 1 {
			l.Infof("Got peer certificate list of length %d != 1 from %s; protocol error", cl, conn.RemoteAddr())
			conn.Close()
			continue
		}
		remoteCert := certs[0]
		remoteID := protocol.NewDeviceID(remoteCert.Raw)

		if remoteID == myID {
			l.Infof("Connected to myself (%s) - should not happen", remoteID)
			conn.Close()
			continue
		}

		if m.ConnectedTo(remoteID) {
			l.Infof("Connected to already connected device (%s)", remoteID)
			conn.Close()
			continue
		}

		for _, deviceCfg := range cfg.Devices {
			if deviceCfg.DeviceID == remoteID {
				// Verify the name on the certificate. By default we set it to
				// "syncthing" when generating, but the user may have replaced
				// the certificate and used another name.
				certName := deviceCfg.CertName
				if certName == "" {
					certName = "syncthing"
				}
				err := remoteCert.VerifyHostname(certName)
				if err != nil {
					// Incorrect certificate name is something the user most
					// likely wants to know about, since it's an advanced
					// config. Warn instead of Info.
					l.Warnf("Bad certificate from %s (%v): %v", remoteID, conn.RemoteAddr(), err)
					conn.Close()
					continue next
				}

				// If rate limiting is set, we wrap the connection in a
				// limiter.
				var wr io.Writer = conn
				if writeRateLimit != nil {
					wr = &limitedWriter{conn, writeRateLimit}
				}

				var rd io.Reader = conn
				if readRateLimit != nil {
					rd = &limitedReader{conn, readRateLimit}
				}

				name := fmt.Sprintf("%s-%s", conn.LocalAddr(), conn.RemoteAddr())
				protoConn := protocol.NewConnection(remoteID, rd, wr, m, name, deviceCfg.Compression)

				l.Infof("Established secure connection to %s at %s", remoteID, name)
				if debugNet {
					l.Debugf("cipher suite %04X", conn.ConnectionState().CipherSuite)
				}
				events.Default.Log(events.DeviceConnected, map[string]string{
					"id":   remoteID.String(),
					"addr": conn.RemoteAddr().String(),
				})

				m.AddConnection(conn, protoConn)
				continue next
			}
		}

		events.Default.Log(events.DeviceRejected, map[string]string{
			"device":  remoteID.String(),
			"address": conn.RemoteAddr().String(),
		})
		l.Infof("Connection from %s with unknown device ID %s; ignoring", conn.RemoteAddr(), remoteID)
		conn.Close()
	}
}

func listenTLS(conns chan *tls.Conn, addr string, tlsCfg *tls.Config) {
	if debugNet {
		l.Debugln("listening on", addr)
	}

	tcaddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		l.Fatalln("listen (BEP):", err)
	}
	listener, err := net.ListenTCP("tcp", tcaddr)
	if err != nil {
		l.Fatalln("listen (BEP):", err)
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			l.Warnln("Accepting connection:", err)
			continue
		}

		if debugNet {
			l.Debugln("connect from", conn.RemoteAddr())
		}

		tcpConn := conn.(*net.TCPConn)
		setTCPOptions(tcpConn)

		tc := tls.Server(conn, tlsCfg)
		err = tc.Handshake()
		if err != nil {
			l.Infoln("TLS handshake:", err)
			tc.Close()
			continue
		}

		conns <- tc
	}

}

func dialTLS(m *model.Model, conns chan *tls.Conn, tlsCfg *tls.Config) {
	var delay time.Duration = 1 * time.Second
	for {
	nextDevice:
		for _, deviceCfg := range cfg.Devices {
			if deviceCfg.DeviceID == myID {
				continue
			}

			if m.ConnectedTo(deviceCfg.DeviceID) {
				continue
			}

			var addrs []string
			for _, addr := range deviceCfg.Addresses {
				if addr == "dynamic" {
					if discoverer != nil {
						t := discoverer.Lookup(deviceCfg.DeviceID)
						if len(t) == 0 {
							continue
						}
						addrs = append(addrs, t...)
					}
				} else {
					addrs = append(addrs, addr)
				}
			}

			for _, addr := range addrs {
				host, port, err := net.SplitHostPort(addr)
				if err != nil && strings.HasPrefix(err.Error(), "missing port") {
					// addr is on the form "1.2.3.4"
					addr = net.JoinHostPort(addr, "22000")
				} else if err == nil && port == "" {
					// addr is on the form "1.2.3.4:"
					addr = net.JoinHostPort(host, "22000")
				}
				if debugNet {
					l.Debugln("dial", deviceCfg.DeviceID, addr)
				}

				raddr, err := net.ResolveTCPAddr("tcp", addr)
				if err != nil {
					if debugNet {
						l.Debugln(err)
					}
					continue
				}

				conn, err := net.DialTCP("tcp", nil, raddr)
				if err != nil {
					if debugNet {
						l.Debugln(err)
					}
					continue
				}

				setTCPOptions(conn)

				tc := tls.Client(conn, tlsCfg)
				err = tc.Handshake()
				if err != nil {
					l.Infoln("TLS handshake:", err)
					tc.Close()
					continue
				}

				conns <- tc
				continue nextDevice
			}
		}

		time.Sleep(delay)
		delay *= 2
		if maxD := time.Duration(cfg.Options.ReconnectIntervalS) * time.Second; delay > maxD {
			delay = maxD
		}
	}
}

func setTCPOptions(conn *net.TCPConn) {
	var err error
	if err = conn.SetLinger(0); err != nil {
		l.Infoln(err)
	}
	if err = conn.SetNoDelay(false); err != nil {
		l.Infoln(err)
	}
	if err = conn.SetKeepAlivePeriod(60 * time.Second); err != nil {
		l.Infoln(err)
	}
	if err = conn.SetKeepAlive(true); err != nil {
		l.Infoln(err)
	}
}

func discovery(extPort int) *discover.Discoverer {
	disc := discover.NewDiscoverer(myID, cfg.Options.ListenAddress)

	if cfg.Options.LocalAnnEnabled {
		l.Infoln("Starting local discovery announcements")
		disc.StartLocal(cfg.Options.LocalAnnPort, cfg.Options.LocalAnnMCAddr)
	}

	if cfg.Options.GlobalAnnEnabled {
		l.Infoln("Starting global discovery announcements")
		disc.StartGlobal(cfg.Options.GlobalAnnServer, uint16(extPort))
	}

	return disc
}

func ensureDir(dir string, mode int) {
	fi, err := os.Stat(dir)
	if os.IsNotExist(err) {
		err := os.MkdirAll(dir, 0700)
		if err != nil {
			l.Fatalln(err)
		}
	} else if mode >= 0 && err == nil && int(fi.Mode()&0777) != mode {
		err := os.Chmod(dir, os.FileMode(mode))
		// This can fail on crappy filesystems, nothing we can do about it.
		if err != nil {
			l.Warnln(err)
		}
	}
}

func getDefaultConfDir() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("LocalAppData"), "Syncthing")

	case "darwin":
		return expandTilde("~/Library/Application Support/Syncthing")

	default:
		if xdgCfg := os.Getenv("XDG_CONFIG_HOME"); xdgCfg != "" {
			return filepath.Join(xdgCfg, "syncthing")
		} else {
			return expandTilde("~/.config/syncthing")
		}
	}
}

func expandTilde(p string) string {
	if p == "~" {
		return getHomeDir()
	}

	p = filepath.FromSlash(p)
	if !strings.HasPrefix(p, fmt.Sprintf("~%c", os.PathSeparator)) {
		return p
	}

	return filepath.Join(getHomeDir(), p[2:])
}

func getHomeDir() string {
	var home string

	switch runtime.GOOS {
	case "windows":
		home = filepath.Join(os.Getenv("HomeDrive"), os.Getenv("HomePath"))
		if home == "" {
			home = os.Getenv("UserProfile")
		}
	default:
		home = os.Getenv("HOME")
	}

	if home == "" {
		l.Fatalln("No home directory found - set $HOME (or the platform equivalent).")
	}

	return home
}

// getFreePort returns a free TCP port fort listening on. The ports given are
// tried in succession and the first to succeed is returned. If none succeed,
// a random high port is returned.
func getFreePort(host string, ports ...int) (int, error) {
	for _, port := range ports {
		c, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
		if err == nil {
			c.Close()
			return port, nil
		}
	}

	c, err := net.Listen("tcp", host+":0")
	if err != nil {
		return 0, err
	}
	addr := c.Addr().(*net.TCPAddr)
	c.Close()
	return addr.Port, nil
}

func overrideGUIConfig(originalCfg config.GUIConfiguration, address, authentication, apikey string) config.GUIConfiguration {
	// Make a copy of the config
	cfg := originalCfg

	if address == "" {
		address = os.Getenv("STGUIADDRESS")
	}

	if address != "" {
		cfg.Enabled = true

		addressParts := strings.SplitN(address, "://", 2)
		switch addressParts[0] {
		case "http":
			cfg.UseTLS = false
		case "https":
			cfg.UseTLS = true
		default:
			l.Fatalln("Unidentified protocol", addressParts[0])
		}
		cfg.Address = addressParts[1]
	}

	if authentication == "" {
		authentication = os.Getenv("STGUIAUTH")
	}

	if authentication != "" {
		authenticationParts := strings.SplitN(authentication, ":", 2)

		hash, err := bcrypt.GenerateFromPassword([]byte(authenticationParts[1]), 0)
		if err != nil {
			l.Fatalln("Invalid GUI password:", err)
		}

		cfg.User = authenticationParts[0]
		cfg.Password = string(hash)
	}

	if apikey == "" {
		apikey = os.Getenv("STGUIAPIKEY")
	}

	if apikey != "" {
		cfg.APIKey = apikey
	}
	return cfg
}

func standbyMonitor() {
	restartDelay := time.Duration(60 * time.Second)
	now := time.Now()
	for {
		time.Sleep(10 * time.Second)
		if time.Since(now) > 2*time.Minute {
			l.Infof("Paused state detected, possibly woke up from standby. Restarting in %v.", restartDelay)

			// We most likely just woke from standby. If we restart
			// immediately chances are we won't have networking ready. Give
			// things a moment to stabilize.
			time.Sleep(restartDelay)

			restart()
			return
		}
		now = time.Now()
	}
}

func autoUpgrade() {
	var skipped bool
	interval := time.Duration(cfg.Options.AutoUpgradeIntervalH) * time.Hour
	for {
		if skipped {
			time.Sleep(interval)
		} else {
			skipped = true
		}

		rel, err := upgrade.LatestRelease(strings.Contains(Version, "-beta"))
		if err != nil {
			l.Warnln("Automatic upgrade:", err)
			continue
		}

		if upgrade.CompareVersions(rel.Tag, Version) <= 0 {
			continue
		}

		l.Infof("Automatic upgrade (current %q < latest %q)", Version, rel.Tag)
		err = upgrade.UpgradeTo(rel, GoArchExtra)
		if err != nil {
			l.Warnln("Automatic upgrade:", err)
			continue
		}
		l.Warnf("Automatically upgraded to version %q. Restarting in 1 minute.", rel.Tag)
		time.Sleep(time.Minute)
		stop <- exitUpgrading
		return
	}
}
