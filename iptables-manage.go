package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"strconv"
	"strings"

	"github.com/fsnotify/fsnotify"
	"github.com/horgh/iptables-manage/cidrlist"
)

// Args holds command line arguments.
type Args struct {
	// Path to file holding list of allowed CIDRs.
	CIDRFile string

	// Ports to grant access to.
	Ports []int

	// Verbose controls verbose output.
	Verbose bool

	// Daemonise. If true, then the program sits persistently and watches the
	// given CIDR file for modifications. When the file is modified, we apply
	// the changes immediately.
	Daemonise bool
}

// IPTablesRule holds an iptables rule.
type IPTablesRule struct {
	// Source CIDR.
	Source *net.IPNet

	// Destination port.
	DestPort int

	// Line number.
	Line int
}

// main is the program entry.
func main() {
	log.SetFlags(0)

	// Ensure we are running as root.
	user, err := user.Current()
	if err != nil {
		log.Fatalf("Unable to determine the current user: %s", err)
	}

	if user.Username != "root" {
		log.Fatalf("You must run this program as root. You are %s.", user.Username)
	}

	args, err := getArgs()
	if err != nil {
		log.Printf("Invalid argument: %s", err)
		flag.PrintDefaults()
		os.Exit(1)
	}

	if err := applyUpdatesFromCIDRFile(args.CIDRFile, args.Verbose, args.Ports); err != nil {
		log.Fatal(err)
	}

	if !args.Daemonise {
		return
	}

	if err := watchCIDRFile(args.CIDRFile, args.Verbose, args.Ports); err != nil {
		log.Fatal(err)
	}
}

// getArgs retrieves and validates command line arguments.
func getArgs() (Args, error) {
	cidrFile := flag.String("cidr-file", "", "File with CIDRs to allow.")
	portsString := flag.String("ports", "80,443", "Port(s) to grant access to. Comma separated.")
	verbose := flag.Bool("verbose", false, "Toggle verbose output.")
	daemonise := flag.Bool("daemonise", false, "Daemonise and watch the CIDR file for changes. Apply changes when the file changes.")

	flag.Parse()

	if len(*cidrFile) == 0 {
		return Args{}, fmt.Errorf("please provide a CIDR file")
	}

	if len(*portsString) == 0 {
		return Args{}, fmt.Errorf("please provide ports")
	}

	portsRaw := strings.Split(*portsString, ",")
	ports := []int{}
	for _, port := range portsRaw {
		port = strings.TrimSpace(port)
		if len(port) == 0 {
			continue
		}

		portInt, err := strconv.Atoi(port)
		if err != nil {
			return Args{}, fmt.Errorf("invalid port: %s: %s", port, err)
		}

		ports = append(ports, portInt)
	}

	return Args{
		CIDRFile:  *cidrFile,
		Ports:     ports,
		Verbose:   *verbose,
		Daemonise: *daemonise,
	}, nil
}

// applyUpdatesFromCIDRFile ensures the iptables rules match what is in the
// CIDR file.
func applyUpdatesFromCIDRFile(cidrFile string, verbose bool,
	ports []int) error {
	// Load CIDRs to be allowed.
	fileRecords, err := cidrlist.LoadCIDRsFromFile(cidrFile)
	if err != nil {
		return fmt.Errorf("unable to load CIDRs: %s", err)
	}

	// Determine CIDRs currently allowed.
	currentRules, err := getCurrentRules(verbose)
	if err != nil {
		return fmt.Errorf("unable to determine current rules: %s", err)
	}

	fileCIDRs := []*net.IPNet{}
	for _, r := range fileRecords {
		fileCIDRs = append(fileCIDRs, r.Net)
	}

	// Remove any that are allowed that should not be.
	if err := removeUnlistedRules(fileCIDRs, ports, currentRules, verbose); err != nil {
		return fmt.Errorf("unable to remove rules that are not in the rule file: %s",
			err)
	}

	// Add any not yet allowed that should be.
	if err := addMissingRules(fileCIDRs, ports, currentRules, verbose); err != nil {
		return fmt.Errorf("unable to add missing rules: %s", err)
	}

	return nil
}

// getCurrentRules runs iptables -L and collects rules into memory.
//
// At this time it only records ACCEPT and tcp rules. It is rather specific
// for what this program currently manages.
//
// It also records each rule's line number.
func getCurrentRules(verbose bool) ([]IPTablesRule, error) {
	cmd := exec.Command("iptables", "-nL", "INPUT", "--line-numbers")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("unable to run iptables -nL: %s", err)
	}

	buf := bytes.NewBuffer(output)
	scanner := bufio.NewScanner(buf)

	rules := []IPTablesRule{}

	for scanner.Scan() {
		pieces := strings.Fields(scanner.Text())
		if len(pieces) != 8 {
			if verbose {
				log.Printf("Ignoring line due to column count %d: %s", len(pieces),
					scanner.Text())
			}
			continue
		}

		num := pieces[0]
		target := pieces[1]
		prot := pieces[2]
		source := pieces[4]
		dpt := pieces[7]

		numInt, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("unable to parse number as integer: %s: %s", num,
				err)
		}

		if target != "ACCEPT" {
			if verbose {
				log.Printf("Ignoring non-ACCEPT rule: %s", scanner.Text())
			}
			continue
		}
		if prot != "tcp" {
			if verbose {
				log.Printf("Ignoring non-tcp rule: %s", scanner.Text())
			}
			continue
		}

		if strings.Index(source, "/") == -1 {
			source = source + "/32"
		}

		_, ipNet, err := net.ParseCIDR(source)
		if err != nil {
			return nil, fmt.Errorf("unable to parse source as CIDR: %s: %s", source,
				err)
		}

		re := regexp.MustCompile("^dpt:(\\d+)$")
		matches := re.FindStringSubmatch(dpt)
		if matches == nil {
			return nil, fmt.Errorf("unexpected dpt value: %s", dpt)
		}

		port, err := strconv.Atoi(matches[1])
		if err != nil {
			return nil, fmt.Errorf("unable to parse port as integer: %s: %s",
				matches[1], err)
		}

		rules = append(rules, IPTablesRule{
			Source:   ipNet,
			DestPort: port,
			Line:     numInt,
		})
	}

	if scanner.Err() != nil {
		return nil, fmt.Errorf("scan error: %s", err)
	}

	return rules, nil
}

// removeUnlistedRules compares active iptables rules with those that should
// be there as defined by the CIDR file CIDRs.
//
// We only look at rules matching one of the specified ports.
//
// If there is an iptables rule that is not one of our listed CIDRs, then
// remove the rule.
func removeUnlistedRules(cidrs []*net.IPNet, ports []int,
	currentRules []IPTablesRule, verbose bool) error {
	// Track how many rules we remove. This lets us know the real line number
	// as we progress through the rules.
	// Note we assume the rules are in order by line number.
	rulesRemoved := 0

	for _, rule := range currentRules {
		// If it is not a port we are managing, then ignore it.
		if !isPortInList(ports, rule.DestPort) {
			continue
		}

		// It is an IP we want listed, so ignore it too since it should be there.
		if isCIDRInList(cidrs, rule.Source) {
			continue
		}

		// It's not wanted! Remove it.

		lineNumber := rule.Line - rulesRemoved
		err := removeRule(lineNumber)
		if err != nil {
			return fmt.Errorf("unable to remove rule: %v: %s", rule, err)
		}

		log.Printf("Removed unwanted rule: %v", rule)
		rulesRemoved++
	}

	return nil
}

// isPortInList looks through a slice of integers. If there is one that matches
// the port number then we say it is in the list.
func isPortInList(ports []int, port int) bool {
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

// isCIDRInList looks through a slice of CIDRs. If the given CIDR matches one,
// then we say it is in the list.
func isCIDRInList(cidrs []*net.IPNet, cidr *net.IPNet) bool {
	for _, c := range cidrs {
		if c.String() == cidr.String() {
			return true
		}
	}
	return false
}

// removeRule calls iptables -D to remove a rule on the given line number.
func removeRule(lineNumber int) error {
	cmd := exec.Command("iptables", "-D", "INPUT", strconv.Itoa(lineNumber))
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("unable to run iptables -D: %s", err)
	}
	return nil
}

// addMissingRules looks at the CIDRs that should have rules.
//
// If there should be a rule for the CIDR and port combination, then we add it.
func addMissingRules(cidrs []*net.IPNet, ports []int,
	currentRules []IPTablesRule, verbose bool) error {
	for _, cidr := range cidrs {
		for _, port := range ports {
			// If it's already listed, then do nothing.
			if isAnActiveRule(cidr, port, currentRules) {
				if verbose {
					log.Printf("Rule already exists: %s %d", cidr, port)
				}
				continue
			}

			err := addRule(cidr, port)
			if err != nil {
				return fmt.Errorf("unable to add rule: %s", err)
			}

			log.Printf("Added rule: %s %d", cidr, port)
		}
	}

	return nil
}

// isAnActiveRule compares the CIDR and port with the slice of IPTables rules.
//
// If the CIDR/port tuple matches a rule, then we say it is active.
func isAnActiveRule(cidr *net.IPNet, port int,
	currentRules []IPTablesRule) bool {
	for _, rule := range currentRules {
		if rule.Source.String() == cidr.String() && rule.DestPort == port {
			return true
		}
	}
	return false
}

// addRule runs iptables -I to add the given CIDR and port tuple.
func addRule(cidr *net.IPNet, port int) error {
	cmd := exec.Command(
		"iptables", "-I", "INPUT", "1",
		"-s", cidr.String(),
		"-p", "tcp",
		"-m", "tcp",
		"--dport", strconv.Itoa(port),
		"-j", "ACCEPT",
	)
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("unable to run iptables -I: %s", err)
	}
	return nil
}

// watchCIDRFile waits for modification events to the CIDR file. When seen,
// we apply the CIDR file to the iptables rules. This loops forever (or
// until we see an error).
func watchCIDRFile(cidrFile string, verbose bool, ports []int) error {
	watcher, err := watchFile(cidrFile)
	if err != nil {
		return fmt.Errorf("unable to watch file: %s", err)
	}

	for {
		if verbose {
			log.Printf("Waiting for changes...")
		}

		select {
		case ev := <-watcher.Events:
			if verbose {
				log.Printf("Event: %s", ev)
			}

			// File removed. Watch again. We expect the file was replaced.
			if ev.Op == fsnotify.Remove {
				if err := watcher.Close(); err != nil {
					return fmt.Errorf("watcher close error: %s", err)
				}

				watcher, err = watchFile(cidrFile)
				if err != nil {
					return fmt.Errorf("unable to re-watch file: %s", err)
				}

				// Fall through. The file was replaced, so run updates.
			}

			if ev.Op == fsnotify.Write || ev.Op == fsnotify.Remove {
				if err := applyUpdatesFromCIDRFile(cidrFile, verbose, ports); err != nil {
					_ = watcher.Close()
					return fmt.Errorf("unable to apply updates: %s", err)
				}

				log.Printf("Applied updates.")
				continue
			}

		case err := <-watcher.Errors:
			_ = watcher.Close()
			return fmt.Errorf("error watching file: %s: %s", cidrFile, err)
		}
	}
}

// watchFile creates a new Watcher watching the given file.
func watchFile(file string) (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("unable to create file watcher: %s", err)
	}

	if err := watcher.Add(file); err != nil {
		_ = watcher.Close()
		return nil, fmt.Errorf("unable to re-watch file: %s: %s", file, err)
	}

	return watcher, nil
}
