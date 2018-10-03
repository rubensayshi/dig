// Copyright (c) 2018 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package dig

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"go.uber.org/dig/internal/digreflect"
	"go.uber.org/dig/internal/dot"
)

const (
	_optionalTag = "optional"
	_nameTag     = "name"
	_groupTag    = "group"
)

// Unique identification of an object in the graph.
type key struct {
	t reflect.Type

	// Only one of name or group will be set.
	name  string
	group string
}

// Option configures a Container. It's included for future functionality;
// currently, there are no concrete implementations.
type Option interface {
	applyOption(*Container)
}

type optionFunc func(*Container)

func (f optionFunc) applyOption(c *Container) { f(c) }

type provideOptions struct {
	Name string
}

func (o *provideOptions) Validate() error {
	// Names must be representable inside a backquoted string. The only
	// limitation for raw string literals as per
	// https://golang.org/ref/spec#raw_string_lit is that they cannot contain
	// backquotes.
	if strings.ContainsRune(o.Name, '`') {
		return fmt.Errorf("invalid dig.Name(%q): names cannot contain backquotes", o.Name)
	}
	return nil
}

// A ProvideOption modifies the default behavior of Provide.
type ProvideOption interface {
	applyProvideOption(*provideOptions)
}

type provideOptionFunc func(*provideOptions)

func (f provideOptionFunc) applyProvideOption(opts *provideOptions) { f(opts) }

// Name is a ProvideOption that specifies that all values produced by a
// constructor should have the given name. See also the package documentation
// about Named Values.
//
// Given,
//
//   func NewReadOnlyConnection(...) (*Connection, error)
//   func NewReadWriteConnection(...) (*Connection, error)
//
// The following will provide two connections to the container: one under the
// name "ro" and the other under the name "rw".
//
//   c.Provide(NewReadOnlyConnection, dig.Name("ro"))
//   c.Provide(NewReadWriteConnection, dig.Name("rw"))
//
// This option cannot be provided for constructors which produce result
// objects.
func Name(name string) ProvideOption {
	return provideOptionFunc(func(opts *provideOptions) {
		opts.Name = name
	})
}

// An InvokeOption modifies the default behavior of Invoke. It's included for
// future functionality; currently, there are no concrete implementations.
type InvokeOption interface {
	unimplemented()
}

// Container is a directed acyclic graph of types and their dependencies.
type Container struct {
	// Mapping from key to all the nodes that can provide a value for that
	// key.
	providers map[key][]*node

	// All nodes in the container.
	nodes []*node

	// Values that have already been generated in the container.
	values map[key]reflect.Value

	// Values groups that have already been generated in the container.
	groups map[key][]reflect.Value

	// Source of randomness.
	rand *rand.Rand

	// Flag indicating whether the graph has been checked for cycles.
	isVerifiedAcyclic bool

	// Defer acyclic check on provide until Invoke.
	deferAcyclicVerification bool
}

// containerWriter provides write access to the Container's underlying data
// store.
type containerWriter interface {
	// setValue sets the value with the given name and type in the container.
	// If a value with the same name and type already exists, it will be
	// overwritten.
	setValue(name string, t reflect.Type, v reflect.Value)

	// submitGroupedValue submits a value to the value group with the provided
	// name.
	submitGroupedValue(name string, t reflect.Type, v reflect.Value)
}

// containerStore provides access to the Container's underlying data store.
type containerStore interface {
	containerWriter

	// Returns a slice containing all known types.
	knownTypes() []reflect.Type

	// Retrieves the value with the provided name and type, if any.
	getValue(name string, t reflect.Type) (v reflect.Value, ok bool)

	// Retrieves all values for the provided group and type.
	//
	// The order in which the values are returned is undefined.
	getValueGroup(name string, t reflect.Type) []reflect.Value

	// Returns the providers that can produce a value with the given name and
	// type.
	getValueProviders(name string, t reflect.Type) []provider

	// Returns the providers that can produce values for the given group and
	// type.
	getGroupProviders(name string, t reflect.Type) []provider

	createGraph() *dot.Graph
}

// provider encapsulates a user-provided constructor.
type provider interface {
	// ID is a unique numerical identifier for this provider.
	ID() dot.CtorID

	// Location returns where this constructor was defined.
	Location() *digreflect.Func

	// ParamList returns information about the direct dependencies of this
	// constructor.
	ParamList() paramList

	// ResultList returns information about the values produced by this
	// constructor.
	ResultList() resultList

	// Calls the underlying constructor, reading values from the
	// containerStore as needed.
	//
	// The values produced by this provider should be submitted into the
	// containerStore.
	Call(containerStore) error
}

// New constructs a Container.
func New(opts ...Option) *Container {
	c := &Container{
		providers: make(map[key][]*node),
		values:    make(map[key]reflect.Value),
		groups:    make(map[key][]reflect.Value),
		rand:      rand.New(rand.NewSource(time.Now().UnixNano())),
	}

	for _, opt := range opts {
		opt.applyOption(c)
	}
	return c
}

// DeferAcyclicVerification is an Option to override the default behavior
// of container.Provide, deferring the dependency graph validation to no longer
// run after each call to container.Provide. The container will instead verify
// the graph on first `Invoke`.
//
// Applications adding providers to a container in a tight loop may experience
// performance improvements by initializing the container with this option.
func DeferAcyclicVerification() Option {
	return optionFunc(func(c *Container) {
		c.deferAcyclicVerification = true
	})
}

// A VisualizeOption modifies the default behavior of Visualize.
type VisualizeOption interface {
	applyVisualizeOption(*visualizeOptions)
}

type visualizeOptions struct {
	VisualizeError error
}

type visualizeOptionFunc func(*visualizeOptions)

func (f visualizeOptionFunc) applyVisualizeOption(opts *visualizeOptions) { f(opts) }

// VisualizeError includes a visualization of the given error in the output of
// Visualize if an error was returned by Invoke or Provide.
//
//   if err := c.Provide(...); err != nil {
//     dig.Visualize(c, w, dig.VisualizeError(err))
//   }
//
// This option has no effect if the error was nil or if it didn't contain any
// information to visualize.
func VisualizeError(err error) VisualizeOption {
	return visualizeOptionFunc(func(opts *visualizeOptions) {
		opts.VisualizeError = err
	})
}

func updateGraph(dg *dot.Graph, err error) error {
	var errors []errVisualizer
	// Unwrap error to find the root cause.
	for {
		if ev, ok := err.(errVisualizer); ok {
			errors = append(errors, ev)
		}
		e, ok := err.(causer)
		if !ok {
			break
		}
		err = e.cause()
	}

	// If there are no errVisualizers included, we do not modify the graph.
	if len(errors) == 0 {
		return nil
	}

	// We iterate in reverse because the last element is the root cause.
	for i := len(errors) - 1; i >= 0; i-- {
		errors[i].updateGraph(dg)
	}

	return nil
}

var _graphTmpl = template.Must(
	template.New("DotGraph").
		Funcs(template.FuncMap{
			"quote": strconv.Quote,
		}).
		Parse(`digraph {
	graph [compound=true];
	{{range $g := .Groups}}
		{{- quote .String}} [{{.Attributes}}];
		{{range .Results}}
			{{- quote $g.String}} -> {{quote .String}};
		{{end}}
	{{end -}}
	{{range $index, $ctor := .Ctors}}
		subgraph cluster_{{$index}} {
			constructor_{{$index}} [shape=plaintext label={{quote .Name}}];
			{{with .ErrorType}}color={{.Color}};{{end}}
			{{range .Results}}
				{{- quote .String}} [{{.Attributes}}];
			{{end}}
		}
		{{range .Params}}
			constructor_{{$index}} -> {{quote .String}} [ltail=cluster_{{$index}}{{if .Optional}} style=dashed{{end}}];
		{{end}}
		{{range .GroupParams}}
			constructor_{{$index}} -> {{quote .String}} [ltail=cluster_{{$index}}];
		{{end -}}
	{{end}}
	{{range .Failed.TransitiveFailures}}
		{{- quote .String}} [color=orange];
	{{end -}}
	{{range .Failed.RootCauses}}
		{{- quote .String}} [color=red];
	{{end}}
}`))

// Visualize parses the graph in Container c into DOT format and writes it to
// io.Writer w.
func Visualize(c *Container, w io.Writer, opts ...VisualizeOption) error {
	dg := c.createGraph()

	var options visualizeOptions
	for _, o := range opts {
		o.applyVisualizeOption(&options)
	}

	if options.VisualizeError != nil {
		if err := updateGraph(dg, options.VisualizeError); err != nil {
			return err
		}
	}

	return _graphTmpl.Execute(w, dg)
}

// CanVisualizeError returns true if the error is an errVisualizer.
func CanVisualizeError(err error) bool {
	for {
		if _, ok := err.(errVisualizer); ok {
			return true
		}
		e, ok := err.(causer)
		if !ok {
			break
		}
		err = e.cause()
	}

	return false
}

func (c *Container) createGraph() *dot.Graph {
	dg := dot.NewGraph()

	for _, n := range c.nodes {
		dg.AddCtor(newDotCtor(n), n.paramList.DotParam(), n.resultList.DotResult())
	}

	return dg
}

// Changes the source of randomness for the container.
//
// This will help provide determinism during tests.
func setRand(r *rand.Rand) Option {
	return optionFunc(func(c *Container) {
		c.rand = r
	})
}

func (c *Container) knownTypes() []reflect.Type {
	typeSet := make(map[reflect.Type]struct{}, len(c.providers))
	for k := range c.providers {
		typeSet[k.t] = struct{}{}
	}

	types := make([]reflect.Type, 0, len(typeSet))
	for t := range typeSet {
		types = append(types, t)
	}
	sort.Sort(byTypeName(types))
	return types
}

func (c *Container) getValue(name string, t reflect.Type) (v reflect.Value, ok bool) {
	v, ok = c.values[key{name: name, t: t}]
	return
}

func (c *Container) setValue(name string, t reflect.Type, v reflect.Value) {
	c.values[key{name: name, t: t}] = v
}

func (c *Container) getValueGroup(name string, t reflect.Type) []reflect.Value {
	items := c.groups[key{group: name, t: t}]
	// shuffle the list so users don't rely on the ordering of grouped values
	return shuffledCopy(c.rand, items)
}

func (c *Container) submitGroupedValue(name string, t reflect.Type, v reflect.Value) {
	k := key{group: name, t: t}
	c.groups[k] = append(c.groups[k], v)
}

func (c *Container) getValueProviders(name string, t reflect.Type) []provider {
	return c.getProviders(key{name: name, t: t})
}

func (c *Container) getGroupProviders(name string, t reflect.Type) []provider {
	return c.getProviders(key{group: name, t: t})
}

func (c *Container) getProviders(k key) []provider {
	nodes := c.providers[k]
	providers := make([]provider, len(nodes))
	for i, n := range nodes {
		providers[i] = n
	}
	return providers
}

// Provide teaches the container how to build values of one or more types and
// expresses their dependencies.
//
// The first argument of Provide is a function that accepts zero or more
// parameters and returns one or more results. The function may optionally
// return an error to indicate that it failed to build the value. This
// function will be treated as the constructor for all the types it returns.
// This function will be called AT MOST ONCE when a type produced by it, or a
// type that consumes this function's output, is requested via Invoke. If the
// same types are requested multiple times, the previously produced value will
// be reused.
//
// In addition to accepting constructors that accept dependencies as separate
// arguments and produce results as separate return values, Provide also
// accepts constructors that specify dependencies as dig.In structs and/or
// specify results as dig.Out structs.
func (c *Container) Provide(constructor interface{}, opts ...ProvideOption) error {
	ctype := reflect.TypeOf(constructor)
	if ctype == nil {
		return errors.New("can't provide an untyped nil")
	}
	if ctype.Kind() != reflect.Func {
		return fmt.Errorf("must provide constructor function, got %v (type %v)", constructor, ctype)
	}

	var options provideOptions
	for _, o := range opts {
		o.applyProvideOption(&options)
	}
	if err := options.Validate(); err != nil {
		return err
	}

	if err := c.provide(constructor, options); err != nil {
		return errProvide{
			Func:   digreflect.InspectFunc(constructor),
			Reason: err,
		}
	}
	return nil
}

// Invoke runs the given function after instantiating its dependencies.
//
// Any arguments that the function has are treated as its dependencies. The
// dependencies are instantiated in an unspecified order along with any
// dependencies that they might have.
//
// The function may return an error to indicate failure. The error will be
// returned to the caller as-is.
func (c *Container) Invoke(function interface{}, providedParams ...interface{}) error {
	ftype := reflect.TypeOf(function)
	if ftype == nil {
		return errors.New("can't invoke an untyped nil")
	}
	if ftype.Kind() != reflect.Func {
		return fmt.Errorf("can't invoke non-function %v (type %v)", function, ftype)
	}

	pl, err := newParamList(ftype, providedParams...)
	if err != nil {
		return err
	}

	if err := shallowCheckDependencies(c, pl); err != nil {
		return errMissingDependencies{
			Func:   digreflect.InspectFunc(function),
			Reason: err,
		}
	}

	if !c.isVerifiedAcyclic {
		if err := c.verifyAcyclic(); err != nil {
			return err
		}
	}

	args, err := pl.BuildList(c)
	if err != nil {
		return errArgumentsFailed{
			Func:   digreflect.InspectFunc(function),
			Reason: err,
		}
	}

	returned := reflect.ValueOf(function).Call(args)
	if len(returned) == 0 {
		return nil
	}
	if last := returned[len(returned)-1]; isError(last.Type()) {
		if err, _ := last.Interface().(error); err != nil {
			return err
		}
	}
	return nil
}

func (c *Container) verifyAcyclic() error {
	visited := make(map[key]struct{})
	for _, n := range c.nodes {
		if err := detectCycles(n, c, nil /* path */, visited); err != nil {
			return errWrapf(err, "cycle detected in dependency graph")
		}
	}

	c.isVerifiedAcyclic = true
	return nil
}

func (c *Container) provide(ctor interface{}, opts provideOptions) error {
	n, err := newNode(ctor, nodeOptions{ResultName: opts.Name})
	if err != nil {
		return err
	}

	keys, err := c.findAndValidateResults(n)
	if err != nil {
		return err
	}

	ctype := reflect.TypeOf(ctor)
	if len(keys) == 0 {
		return fmt.Errorf("%v must provide at least one non-error type", ctype)
	}

	for k := range keys {
		c.isVerifiedAcyclic = false
		oldProviders := c.providers[k]
		c.providers[k] = append(c.providers[k], n)

		if c.deferAcyclicVerification {
			continue
		}
		if err := verifyAcyclic(c, n, k); err != nil {
			c.providers[k] = oldProviders
			return err
		}
		c.isVerifiedAcyclic = true
	}

	c.nodes = append(c.nodes, n)

	return nil
}

// Builds a collection of all result types produced by this node.
func (c *Container) findAndValidateResults(n *node) (map[key]struct{}, error) {
	var err error
	keyPaths := make(map[key]string)
	walkResult(n.ResultList(), connectionVisitor{
		c:        c,
		n:        n,
		err:      &err,
		keyPaths: keyPaths,
	})

	if err != nil {
		return nil, err
	}

	keys := make(map[key]struct{}, len(keyPaths))
	for k := range keyPaths {
		keys[k] = struct{}{}
	}
	return keys, nil
}

// Visits the results of a node and compiles a collection of all the keys
// produced by that node.
type connectionVisitor struct {
	c *Container
	n *node

	// If this points to a non-nil value, we've already encountered an error
	// and should stop traversing.
	err *error

	// Map of keys provided to path that provided this. The path is a string
	// documenting which positional return value or dig.Out attribute is
	// providing this particular key.
	//
	// For example, "[0].Foo" indicates that the value was provided by the Foo
	// attribute of the dig.Out returned as the first result of the
	// constructor.
	keyPaths map[key]string

	// We track the path to the current result here. For example, this will
	// be, ["[1]", "Foo", "Bar"] when we're visiting Bar in,
	//
	//   func() (io.Writer, struct {
	//     dig.Out
	//
	//     Foo struct {
	//       dig.Out
	//
	//       Bar io.Reader
	//     }
	//   })
	currentResultPath []string
}

func (cv connectionVisitor) AnnotateWithField(f resultObjectField) resultVisitor {
	cv.currentResultPath = append(cv.currentResultPath, f.FieldName)
	return cv
}

func (cv connectionVisitor) AnnotateWithPosition(i int) resultVisitor {
	cv.currentResultPath = append(cv.currentResultPath, fmt.Sprintf("[%d]", i))
	return cv
}

func (cv connectionVisitor) Visit(res result) resultVisitor {
	// Already failed. Stop looking.
	if *cv.err != nil {
		return nil
	}

	path := strings.Join(cv.currentResultPath, ".")

	switch r := res.(type) {
	case resultSingle:
		k := key{name: r.Name, t: r.Type}

		if conflict, ok := cv.keyPaths[k]; ok {
			*cv.err = fmt.Errorf(
				"cannot provide %v from %v: already provided by %v",
				k, path, conflict)
			return nil
		}

		if ps := cv.c.providers[k]; len(ps) > 0 {
			cons := make([]string, len(ps))
			for i, p := range ps {
				cons[i] = fmt.Sprint(p.Location())
			}

			*cv.err = fmt.Errorf(
				"cannot provide %v from %v: already provided by %v",
				k, path, strings.Join(cons, "; "))
			return nil
		}

		cv.keyPaths[k] = path

	case resultGrouped:
		// we don't really care about the path for this since conflicts are
		// okay for group results. We'll track it for the sake of having a
		// value there.
		k := key{group: r.Group, t: r.Type}
		cv.keyPaths[k] = path
	}

	return cv
}

// node is a node in the dependency graph. Each node maps to a single
// constructor provided by the user.
//
// Nodes can produce zero or more values that they store into the container.
// For the Provide path, we verify that nodes produce at least one value,
// otherwise the function will never be called.
type node struct {
	ctor  interface{}
	ctype reflect.Type

	// Location where this function was defined.
	location *digreflect.Func

	// id uniquely identifies the constructor that produces a node.
	id dot.CtorID

	// Whether the constructor owned by this node was already called.
	called bool

	// Type information about constructor parameters.
	paramList paramList

	// Type information about constructor results.
	resultList resultList
}

type nodeOptions struct {
	// If specified, all values produced by this node have the provided name.
	ResultName string
}

func newNode(ctor interface{}, opts nodeOptions) (*node, error) {
	cval := reflect.ValueOf(ctor)
	ctype := cval.Type()
	cptr := cval.Pointer()

	params, err := newParamList(ctype)
	if err != nil {
		return nil, err
	}

	results, err := newResultList(ctype, resultOptions{Name: opts.ResultName})
	if err != nil {
		return nil, err
	}

	return &node{
		ctor:       ctor,
		ctype:      ctype,
		location:   digreflect.InspectFunc(ctor),
		id:         dot.CtorID(cptr),
		paramList:  params,
		resultList: results,
	}, err
}

func (n *node) Location() *digreflect.Func { return n.location }
func (n *node) ParamList() paramList       { return n.paramList }
func (n *node) ResultList() resultList     { return n.resultList }
func (n *node) ID() dot.CtorID             { return n.id }

// Call calls this node's constructor if it hasn't already been called and
// injects any values produced by it into the provided container.
func (n *node) Call(c containerStore) error {
	if n.called {
		return nil
	}

	if err := shallowCheckDependencies(c, n.paramList); err != nil {
		return errMissingDependencies{
			Func:   n.location,
			Reason: err,
		}
	}

	args, err := n.paramList.BuildList(c)
	if err != nil {
		return errArgumentsFailed{
			Func:   n.location,
			Reason: err,
		}
	}

	receiver := newStagingContainerWriter()
	results := reflect.ValueOf(n.ctor).Call(args)
	if err := n.resultList.ExtractList(receiver, results); err != nil {
		return errConstructorFailed{Func: n.location, Reason: err}
	}
	receiver.Commit(c)
	n.called = true
	return nil
}

// Checks if a field of an In struct is optional.
func isFieldOptional(f reflect.StructField) (bool, error) {
	tag := f.Tag.Get(_optionalTag)
	if tag == "" {
		return false, nil
	}

	optional, err := strconv.ParseBool(tag)
	if err != nil {
		err = errWrapf(err,
			"invalid value %q for %q tag on field %v",
			tag, _optionalTag, f.Name)
	}

	return optional, err
}

// Checks that all direct dependencies of the provided param are present in
// the container. Returns an error if not.
func shallowCheckDependencies(c containerStore, p param) error {
	var missing errMissingManyTypes
	var addMissingNodes []*dot.Param
	walkParam(p, paramVisitorFunc(func(p param) bool {
		ps, ok := p.(paramSingle)
		if !ok {
			return true
		}

		if ns := c.getValueProviders(ps.Name, ps.Type); len(ns) == 0 && !ps.Optional {
			missing = append(missing, newErrMissingType(c, key{name: ps.Name, t: ps.Type}))
			addMissingNodes = append(addMissingNodes, ps.DotParam()...)
		}

		return true
	}))

	if len(missing) > 0 {
		return missing
	}
	return nil
}

// stagingContainerWriter is a containerWriter that records the changes that
// would be made to a containerWriter and defers them until Commit is called.
type stagingContainerWriter struct {
	values map[key]reflect.Value
	groups map[key][]reflect.Value
}

var _ containerWriter = (*stagingContainerWriter)(nil)

func newStagingContainerWriter() *stagingContainerWriter {
	return &stagingContainerWriter{
		values: make(map[key]reflect.Value),
		groups: make(map[key][]reflect.Value),
	}
}

func (sr *stagingContainerWriter) setValue(name string, t reflect.Type, v reflect.Value) {
	sr.values[key{t: t, name: name}] = v
}

func (sr *stagingContainerWriter) submitGroupedValue(group string, t reflect.Type, v reflect.Value) {
	k := key{t: t, group: group}
	sr.groups[k] = append(sr.groups[k], v)
}

// Commit commits the received results to the provided containerWriter.
func (sr *stagingContainerWriter) Commit(cw containerWriter) {
	for k, v := range sr.values {
		cw.setValue(k.name, k.t, v)
	}

	for k, vs := range sr.groups {
		for _, v := range vs {
			cw.submitGroupedValue(k.group, k.t, v)
		}
	}
}

type byTypeName []reflect.Type

func (bs byTypeName) Len() int {
	return len(bs)
}

func (bs byTypeName) Less(i int, j int) bool {
	return fmt.Sprint(bs[i]) < fmt.Sprint(bs[j])
}

func (bs byTypeName) Swap(i int, j int) {
	bs[i], bs[j] = bs[j], bs[i]
}

func shuffledCopy(rand *rand.Rand, items []reflect.Value) []reflect.Value {
	newItems := make([]reflect.Value, len(items))
	for i, j := range rand.Perm(len(items)) {
		newItems[i] = items[j]
	}
	return newItems
}

func newDotCtor(n *node) *dot.Ctor {
	return &dot.Ctor{
		ID:      n.id,
		Name:    n.location.Name,
		Package: n.location.Package,
		File:    n.location.File,
		Line:    n.location.Line,
	}
}
