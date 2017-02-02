// Copyright (2017) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

// miniplumber is a package to facilitate communication pipelines between
// registered readers and writers across a distributed meshage environment.
// miniplumber supports plumbing to external programs over stdio, similar to
// unix pipelines, supports trees of communication pipelines, and configurable
// delivery options.
package miniplumber

import (
	"bufio"
	"fmt"
	"meshage"
	log "minilog"
	"os/exec"
	"sort"
	"strings"
	"sync"
)

// const (
// 	ALL = iota
// 	RR
// 	RND
// )

type Plumber struct {
	node      *meshage.Node         // meshage node to use for distributed environments
	Messages  chan *meshage.Message // incoming messages from meshage
	pipes     map[string]*Pipe
	pipelines map[string]*pipeline
	lock      sync.Mutex
}

type Pipe struct {
	Name       string
	Mode       int
	readers    []*Reader
	numWriters int
	lock       sync.Mutex
}

type Reader struct {
	C    chan string
	Done chan struct{}
	once sync.Once
}

type pipeline struct {
	name       string
	production []string
	done       chan bool
	canceler   sync.Once
}

type Message struct {
}

func (r *Reader) Close() {
	r.once.Do(func() {
		close(r.Done)
	})
}

// New returns a new Plumber object over meshage node n
func New(n *meshage.Node) *Plumber {
	p := &Plumber{
		node:      n,
		Messages:  make(chan *meshage.Message, 1024),
		pipes:     make(map[string]*Pipe),
		pipelines: make(map[string]*pipeline),
	}

	//go p.handleMessages()

	return p
}

func (p *Plumber) Plumb(production ...string) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	// pipelines are unique by their string name, which may be an issue
	// down the road
	name := strings.Join(production, " ")

	log.Debug("got production: %v", name)

	if _, ok := p.pipelines[name]; ok {
		return fmt.Errorf("pipeline already exists: %v", production)
	}

	p.pipelines[name] = &pipeline{
		name:       name,
		production: production,
	}

	go p.startPipeline(p.pipelines[name])

	return nil
}

// func (p *Plumber) Mode(pipe string, mode int) {}

func (p *Plumber) PipelineDelete(production ...string) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	name := strings.Join(production, " ")
	if pp, ok := p.pipelines[name]; !ok {
		return fmt.Errorf("no such pipeline: %v", production)
	} else {
		pp.cancel()
		return nil
	}
}

func (p *Plumber) PipelineDeleteAll() error {
	p.lock.Lock()

	var keys []string
	for k, _ := range p.pipelines {
		keys = append(keys, k)
	}

	p.lock.Unlock()

	for _, k := range keys {
		err := p.PipelineDelete(k)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Plumber) PipeDelete(pipe string) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	if pp, ok := p.pipes[pipe]; ok {
		pp.lock.Lock()
		defer pp.lock.Unlock()

		if pp.numWriters != 0 {
			return fmt.Errorf("cannot delete named pipe with open writers")
		}

		// kill all of the readers
		for _, c := range pp.readers {
			c.Close()
		}
		delete(p.pipes, pipe)

		return nil
	} else {
		return fmt.Errorf("no such pipe: %v", pipe)
	}
}

func (p *Plumber) PipeDeleteAll() error {
	p.lock.Lock()

	var keys []string
	for k, _ := range p.pipes {
		keys = append(keys, k)
	}

	p.lock.Unlock()

	for _, k := range keys {
		err := p.PipeDelete(k)
		if err != nil {
			return err
		}
	}
	return nil
}

// func (p *Plumber) Pipes() ([]*Pipe, error) {
// 	return nil, nil
// }

// Pipelines returns a sorted list of pipeline production strings
func (p *Plumber) Pipelines() []string {
	p.lock.Lock()
	p.lock.Unlock()

	var ret []string

	for k, _ := range p.pipelines {
		ret = append(ret, k)
	}

	sort.Strings(ret)

	return ret
}

func (p *Plumber) NewReader(pipe string) *Reader {
	p.lock.Lock()
	defer p.lock.Unlock()

	return p.newReader(pipe)
}

// assume the lock is held
func (p *Plumber) newReader(pipe string) *Reader {
	log.Debug("newReader: %v", pipe)

	r := &Reader{
		C:    make(chan string),
		Done: make(chan struct{}),
	}

	if pp, ok := p.pipes[pipe]; !ok {
		p.pipes[pipe] = &Pipe{
			Name:    pipe,
			readers: []*Reader{r},
		}
	} else {
		pp.readers = append(pp.readers, r)
	}

	return r
}

func (p *Plumber) NewWriter(pipe string) chan<- string {
	p.lock.Lock()
	defer p.lock.Unlock()

	return p.newWriter(pipe)
}

// assume the lock is held
func (p *Plumber) newWriter(pipe string) chan<- string {
	log.Debug("newWriter: %v", pipe)

	c := make(chan string)

	if _, ok := p.pipes[pipe]; !ok {
		p.pipes[pipe] = &Pipe{
			Name: pipe,
		}
	}
	pp := p.pipes[pipe]
	pp.lock.Lock()
	pp.numWriters++
	pp.lock.Unlock()

	go func() {
		for v := range c {
			pp.write(v)
		}
		pp.lock.Lock()
		pp.numWriters--
		pp.lock.Unlock()
	}()

	return c
}

func (p *Plumber) Write(pipe string, value string) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	if pp, ok := p.pipes[pipe]; ok {
		pp.write(value)
		return nil
	}
	return fmt.Errorf("no such pipe: %v", pipe)
}

// started in a goroutine, don't assume the lock is held
func (p *Plumber) startPipeline(pl *pipeline) {
	pl.done = make(chan bool)

	go func() {
		<-pl.done
		p.lock.Lock()
		delete(p.pipelines, pl.name)
		p.lock.Unlock()
	}()

	var b <-chan string
	for i, e := range pl.production {
		log.Debug("starting pipeline production element: %v", e)

		// start a process if it looks like a process, otherwise create
		// a pipe
		f := fieldsQuoteEscape("\"", e)
		process, err := exec.LookPath(f[0])
		if err == nil {
			f[0] = process

			// don't write data on stdout/err if this is the last stage
			var write bool
			if i != len(pl.production)-1 {
				write = true
			}

			b, err = pl.exec(f, b, write)
			if err != nil {
				pl.cancel()
				log.Errorln(err)
				break
			}
			continue
		}

		// looks like a named pipe
		var in *Reader

		// don't produce output if this is the final stage
		if i != len(pl.production)-1 {
			in = p.NewReader(e)
		}

		var out chan<- string

		// don't produce input if this is the first stage
		if i != 0 {
			out = p.NewWriter(e)
		}
		b = pl.pipe(in, out, b)
	}
}

func (pl *pipeline) exec(production []string, in <-chan string, write bool) (<-chan string, error) {
	log.Debug("exec: %v, %v", production, write)

	var out chan string

	cmd := &exec.Cmd{
		Path: production[0],
		Args: production,
	}

	if in != nil {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}

		go func() {
			defer pl.cancel()

			for {
				select {
				case v := <-in:
					_, err := stdin.Write([]byte(v))
					if err != nil {
						log.Errorln(err)
						return
					}
				case <-pl.done:
					return
				}
			}
		}()
	}

	if write {
		out = make(chan string)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}

		go func() {
			defer pl.cancel()

			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				select {
				case out <- scanner.Text() + "\n":
				case <-pl.done:
					return
				}
				log.Debug("exec got: %v", scanner.Text())
			}
			if err := scanner.Err(); err != nil {
				log.Errorln(err)
				return
			}
		}()
	}

	err := cmd.Start()
	if err != nil {
		return nil, err
	}

	// command is running

	go func() {
		<-pl.done
		cmd.Process.Kill()
	}()

	return out, nil
}
func (pl *pipeline) pipe(pin *Reader, pout chan<- string, in <-chan string) <-chan string {
	log.Debug("pipe")

	if in != nil {
		go func() {
			defer close(pout)
			defer pl.cancel()

			for {
				select {
				case v := <-in:
					select {
					case pout <- v:
					case <-pl.done:
						return
					}
					log.Debug("pipe got: %v", v)
				case <-pl.done:
					return
				}
			}
		}()
	}

	if pin != nil {
		go func() {
			defer pin.Close()
			<-pl.done
		}()
		return pin.C
	}

	return nil
}

func (pl *pipeline) cancel() {
	pl.canceler.Do(func() {
		log.Debug("closing pipeline: %v", pl.name)
		close(pl.done)
	})
}

// don't assume the plumber lock is held
func (p *Pipe) write(value string) {
	p.lock.Lock()
	defer p.lock.Unlock()

	var cull []int
	for i, c := range p.readers {
		log.Debug("write: %v", value)
		select {
		case <-c.Done:
			close(c.C)
			cull = append(cull, i)
		case c.C <- value:
		}
	}

	// remove dead readers
	for i := len(cull) - 1; i >= 0; i-- {
		idx := cull[i]
		log.Debug("removing dead reader idx: %v", idx)
		p.readers = append(p.readers[:idx], p.readers[idx+1:]...)
	}
}

// Return a slice of strings, split on whitespace, not unlike strings.Fields(),
// except that quoted fields are grouped.
// 	Example: a b "c d"
// 	will return: ["a", "b", "c d"]
func fieldsQuoteEscape(c string, input string) []string {
	log.Debug("fieldsQuoteEscape splitting on %v: %v", c, input)
	f := strings.Fields(input)
	var ret []string
	trace := false
	temp := ""

	for _, v := range f {
		if trace {
			if strings.Contains(v, c) {
				trace = false
				temp += " " + trimQuote(c, v)
				ret = append(ret, temp)
			} else {
				temp += " " + v
			}
		} else if strings.Contains(v, c) {
			temp = trimQuote(c, v)
			if strings.HasSuffix(v, c) {
				// special case, single word like 'foo'
				ret = append(ret, temp)
			} else {
				trace = true
			}
		} else {
			ret = append(ret, v)
		}
	}
	log.Debug("generated: %#v", ret)
	return ret
}

func trimQuote(c string, input string) string {
	if c == "" {
		log.Errorln("cannot trim empty space")
		return ""
	}
	var ret string
	for _, v := range input {
		if v != rune(c[0]) {
			ret += string(v)
		}
	}
	return ret
}
