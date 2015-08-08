/*
git-remote-helper implements a git-remote helper that uses the ipfs transport.

TODO

Currently assumes a IPFS Daemon at localhost:5001

Not completed: Push, IPNS, URLs like ipfs::path/.., embedded IPFS node

...

 $ git clone ipfs://$hash/repo.git
 $ cd repo && make $stuff
 $ git commit -a -m 'done!'
 $ git push origin
 => clone-able as ipfs://$newHash/repo.git

Links

https://ipfs.io

https://github.com/whyrusleeping/git-ipfs-rehost

https://git-scm.com/docs/gitremote-helpers
*/
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/cryptix/go/debug"
	"github.com/cryptix/go/logging"
	"github.com/ipfs/go-ipfs-shell"
	"gopkg.in/errgo.v1"
)

const usageMsg = `usage git-remote-ipfs <repository> [<URL>]
supports ipfs://$hash/path..

`

func usage() {
	fmt.Fprint(os.Stderr, usageMsg)
	os.Exit(2)
}

var (
	ipfsShell    = shell.NewShell("localhost:5001")
	ipfsRepoPath string
	thisGitRepo  string
	errc         chan<- error
	log          = logging.Logger("git-remote-ipfs")
)

func main() {
	// logging
	logging.SetupLogging(nil)

	// env var and arguments
	thisGitRepo := os.Getenv("GIT_DIR")
	if thisGitRepo == "" {
		log.Fatal("could not get GIT_DIR env var")
	}
	log.Debug("GIT_DIR=", thisGitRepo)

	var u string // repo url
	v := len(os.Args[1:])
	switch v {
	case 2:
		log.Debug("repo:", os.Args[1])
		log.Debug("url:", os.Args[2])
		u = os.Args[2]
	default:
		log.Fatalf("usage: unknown # of args: %d\n%v", v, os.Args[1:])
	}

	// parse passed URL
	repoUrl, err := url.Parse(u)
	if err != nil {
		log.Fatalf("url.Parse() failed: %s", err)
	}
	if repoUrl.Scheme != "ipfs" { // ipns will have a seperate helper(?)
		log.Fatal("only ipfs schema is supported")
	}
	ipfsRepoPath = fmt.Sprintf("/ipfs/%s/%s", repoUrl.Host, repoUrl.Path)

	// interrupt / error handling
	ec := make(chan error)
	errc = ec
	go func() {
		errc <- interrupt()
	}()

	go speakGit(os.Stdin, os.Stdout)
	if err = <-ec; err != nil {
		log.Error("closing error:", err)
	}
}

// speakGit acts like a git-remote-helper
// see this for more: https://www.kernel.org/pub/software/scm/git/docs/gitremote-helpers.html
func speakGit(r io.Reader, w io.Writer) {
	r = debug.NewReadLogger("git>>", r)
	w = debug.NewWriteLogger("git<<", w)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		text := scanner.Text()
		log.WithField("text", text).Debug("git input")
		switch {

		case text == "capabilities":
			fmt.Fprintln(w, "fetch")
			fmt.Fprintln(w, "push")
			fmt.Fprintln(w, "")

		case strings.HasPrefix(text, "list"):
			log.Debug("got list line")
			refsCat, err := ipfsShell.Cat(filepath.Join(ipfsRepoPath, "info", "refs"))
			if err != nil {
				errc <- errgo.Notef(err, "failed to cat info/refs from %s", ipfsRepoPath)
				return
			}
			ref2hash := make(map[string]string)
			s := bufio.NewScanner(refsCat)
			for s.Scan() {
				hashRef := strings.Split(s.Text(), "\t")
				if len(hashRef) != 2 {
					errc <- errgo.Newf("processing info/refs: what is this: %v", hashRef)
					return
				}
				ref2hash[hashRef[1]] = hashRef[0]
				log.WithField("ref", hashRef[1]).WithField("sha1", hashRef[0]).Debug("got ref")
			}
			if err := s.Err(); err != nil {
				errc <- errgo.Notef(err, "ipfs.Cat(info/refs) scanner error")
				return
			}
			headCat, err := ipfsShell.Cat(filepath.Join(ipfsRepoPath, "HEAD"))
			if err != nil {
				errc <- errgo.Notef(err, "failed to cat HEAD from %s", ipfsRepoPath)
				return
			}
			head, err := ioutil.ReadAll(headCat)
			if err != nil {
				errc <- errgo.Notef(err, "failed to readAll HEAD from %s", ipfsRepoPath)
				return
			}
			if !bytes.HasPrefix(head, []byte("ref: ")) {
				errc <- errgo.Newf("illegal HEAD file from %s: %q", ipfsRepoPath, head)
				return
			}
			headRef := string(bytes.TrimSpace(head[5:]))
			headHash, ok := ref2hash[headRef]
			if !ok {
				// use first hash in map?..
				errc <- errgo.Newf("unknown HEAD reference %q", headRef)
				return
			}
			log.WithField("ref", headRef).WithField("sha1", headHash).Debug("got HEAD ref")

			// output
			fmt.Fprintf(w, "%s HEAD\n", headHash)
			for ref, hash := range ref2hash {
				fmt.Fprintf(w, "%s %s\n", hash, ref)
			}
			fmt.Fprintln(w, "")

		case strings.HasPrefix(text, "fetch "):
			fetchSplit := strings.Split(text, " ")
			if len(fetchSplit) < 2 {
				errc <- errgo.Newf("malformed 'fetch' command. %q", text)
				return
			}
			log.WithFields(map[string]interface{}{
				"sha1": fetchSplit[1],
				"name": fetchSplit[2],
			}).Info("fetch")
			err := fetchObject(fetchSplit[1])
			if err == nil {
				log.Info("fetchObject() worked")
				fmt.Fprintln(w, "")
				continue
			}
			log.WithField("err", err).Error("fetchObject failed")
			err = fetchPackedObject(fetchSplit[1])
			if err != nil {
				errc <- errgo.Notef(err, "fetchPackedObject() failed")
				return
			}

		case text == "":
			log.Warning("got empty line (end of fetch batch?)")
			fmt.Fprintln(w, "")
			fmt.Fprintln(w, "")
			os.Exit(0)

		default:
			errc <- errgo.Newf("Error: default git speak: %q", text)
			return
		}
	}

	if err := scanner.Err(); err != nil {
		errc <- errgo.Notef(err, "scanner.Err()")
		return
	}

	log.Info("speakGit: exited read loop")
	errc <- nil
}
