package eval

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/TacFlow/tacdb/internal/errs"
	"github.com/TacFlow/tacdb/internal/query/ast"
	"github.com/TacFlow/tacdb/internal/query/qv"
	"github.com/TacFlow/tacdb/value"
)

// Func is a function of the dialect (SPEC §15.1). The table is closed:
// names are matched without regard to ASCII case, and the arity is checked
// when a query is compiled.
type Func struct {
	Name      string // canonical spelling
	Min, Max  int    // number of arguments; Max < 0 is unbounded
	Aggregate bool
	Result    qv.Type // static type of the result
	// scalar computes the function on evaluated arguments. Every
	// argument is evaluated, and a Null argument gives Null before scalar
	// is called (Null in, Null out), except for coalesce.
	scalar func(f *frame, e *ast.FuncCall, args []qv.Value) (qv.Value, error)
}

var funcs = map[string]*Func{}

func register(fns ...*Func) {
	for _, fn := range fns {
		funcs[fold(fn.Name)] = fn
	}
}

// fold lowers ASCII letters only, so that no Unicode case rule can make
// two names meet.
func fold(name string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, name)
}

// Lookup returns the function of a name.
func Lookup(name string) (*Func, bool) {
	fn, ok := funcs[fold(name)]
	return fn, ok
}

// Funcs returns the table, sorted by name.
func Funcs() []*Func {
	out := make([]*Func, 0, len(funcs))
	for _, fn := range funcs {
		out = append(out, fn)
	}
	slices.SortFunc(out, func(a, b *Func) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func init() {
	register(
		&Func{Name: "id", Min: 1, Max: 1, Result: qv.TInt | qv.TNull, scalar: fnID},
		&Func{Name: "coalesce", Min: 0, Max: -1, Result: qv.TAny},
		&Func{Name: "abs", Min: 1, Max: 1, Result: qv.TNumber | qv.TNull, scalar: fnAbs},
		&Func{Name: "size", Min: 1, Max: 1, Result: qv.TInt | qv.TNull, scalar: fnSize},
		&Func{Name: "toInteger", Min: 1, Max: 1, Result: qv.TInt | qv.TNull, scalar: fnToInteger},
		&Func{Name: "toFloat", Min: 1, Max: 1, Result: qv.TFloat | qv.TNull, scalar: fnToFloat},
		&Func{Name: "toString", Min: 1, Max: 1, Result: qv.TString | qv.TNull, scalar: fnToString},
		&Func{Name: "toLower", Min: 1, Max: 1, Result: qv.TString | qv.TNull, scalar: fnCase(false)},
		&Func{Name: "toUpper", Min: 1, Max: 1, Result: qv.TString | qv.TNull, scalar: fnCase(true)},
		&Func{Name: "trim", Min: 1, Max: 1, Result: qv.TString | qv.TNull, scalar: fnTrim},
		&Func{Name: "substring", Min: 2, Max: 3, Result: qv.TString | qv.TNull, scalar: fnSubstring},
		&Func{Name: "replace", Min: 3, Max: 3, Result: qv.TString | qv.TNull, scalar: fnReplace},
		&Func{Name: "split", Min: 2, Max: 2, Result: qv.TList | qv.TNull, scalar: fnSplit},
		&Func{Name: "head", Min: 1, Max: 1, Result: qv.TAny, scalar: fnHead},
		&Func{Name: "tail", Min: 1, Max: 1, Result: qv.TList | qv.TNull, scalar: fnTail},
		&Func{Name: "last", Min: 1, Max: 1, Result: qv.TAny, scalar: fnLast},
		&Func{Name: "range", Min: 2, Max: 3, Result: qv.TList | qv.TNull, scalar: fnRange},
		&Func{Name: "type", Min: 1, Max: 1, Result: qv.TString | qv.TNull, scalar: fnType},
		&Func{Name: "labels", Min: 1, Max: 1, Result: qv.TList | qv.TNull, scalar: fnLabels},
		&Func{Name: "properties", Min: 1, Max: 1, Result: qv.TMap | qv.TNull, scalar: fnProperties},

		&Func{Name: "count", Min: 0, Max: 1, Aggregate: true, Result: qv.TInt},
		&Func{Name: "sum", Min: 1, Max: 1, Aggregate: true, Result: qv.TNumber},
		&Func{Name: "avg", Min: 1, Max: 1, Aggregate: true, Result: qv.TFloat | qv.TNull},
		&Func{Name: "min", Min: 1, Max: 1, Aggregate: true, Result: qv.TAny},
		&Func{Name: "max", Min: 1, Max: 1, Aggregate: true, Result: qv.TAny},
		&Func{Name: "collect", Min: 1, Max: 1, Aggregate: true, Result: qv.TList},
	)
}

// call compiles a function call. An aggregate reads the slot where the
// executor puts its result.
func (c *compiler) call(e *ast.FuncCall) (node, error) {
	fn, ok := Lookup(e.Name)
	if !ok {
		return nil, c.fail(e.Span, "unknown function %s", ast.Name(e.Name))
	}
	if err := CheckCall(fn, e, c.src); err != nil {
		return nil, err
	}
	if fn.Aggregate {
		slot, ok := c.scope.Aggregate(e)
		if !ok {
			return nil, c.fail(e.Span, "aggregate %s is not allowed here", fn.Name)
		}
		return func(f *frame) (qv.Value, error) { return f.row.Slot(slot), nil }, nil
	}
	args, err := c.exprs(e.Args)
	if err != nil {
		return nil, err
	}
	if fn.scalar == nil { // coalesce: the first non-Null argument, lazily
		return func(f *frame) (qv.Value, error) {
			for _, a := range args {
				v, err := a(f)
				if err != nil || !v.IsNull() {
					return v, err
				}
			}
			return qv.Value{}, nil
		}, nil
	}
	return func(f *frame) (qv.Value, error) {
		vals := make([]qv.Value, len(args))
		null := false
		for i, a := range args {
			v, err := a(f)
			if err != nil {
				return qv.Value{}, err
			}
			vals[i] = v
			null = null || v.IsNull()
		}
		if null {
			return qv.Value{}, nil
		}
		v, err := fn.scalar(f, e, vals)
		if err != nil {
			return qv.Value{}, f.at(e.Span, err)
		}
		return v, nil
	}, nil
}

// CheckCall checks the arity of a call and where DISTINCT and * apply;
// errors are QUERY_SEMANTIC.
func CheckCall(fn *Func, e *ast.FuncCall, src string) error {
	c := compiler{src: src}
	switch {
	case e.Star && fn.Name != "count":
		return c.fail(e.Span, "%s(*) is not a function call; only count(*) is", fn.Name)
	case e.Star && e.Distinct:
		return c.fail(e.Span, "count(DISTINCT *) is not allowed")
	case e.Distinct && !fn.Aggregate:
		return c.fail(e.Span, "DISTINCT applies only to aggregates, not to %s", fn.Name)
	case len(e.Args) < fn.Min || (fn.Max >= 0 && len(e.Args) > fn.Max):
		return c.fail(e.Span, "%s takes %s, got %d", fn.Name, arity(fn), len(e.Args))
	}
	return nil
}

func arity(fn *Func) string {
	switch {
	case fn.Max < 0:
		return strconv.Itoa(fn.Min) + " or more arguments"
	case fn.Min == fn.Max && fn.Min == 1:
		return "1 argument"
	case fn.Min == fn.Max:
		return strconv.Itoa(fn.Min) + " arguments"
	}
	return strconv.Itoa(fn.Min) + " to " + strconv.Itoa(fn.Max) + " arguments"
}

func mismatch(f *frame, e *ast.FuncCall, v qv.Value) error {
	return f.fail(e.Span, errs.CodeTypeMismatch, "%s does not accept %s", e.Name, v.Kind())
}

func fnID(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
	switch a[0].Kind() {
	case qv.KindNode, qv.KindEdge:
		return qv.Int(int64(a[0].ID())), nil // IDs stay below MaxInt64 (SPEC §31.4)
	}
	return qv.Value{}, mismatch(f, e, a[0])
}

func fnAbs(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
	if i, ok := a[0].AsInt(); ok {
		if i == math.MinInt64 {
			return qv.Value{}, f.fail(e.Span, errs.CodeInvalidArgument, "integer overflow")
		}
		return qv.Int(max(i, -i)), nil
	}
	if x, ok := a[0].AsFloat(); ok {
		return qv.Float(math.Abs(x)), nil
	}
	return qv.Value{}, mismatch(f, e, a[0])
}

// fnSize is the number of code points of a String, bytes of Bytes, items
// of a List and edges of a path.
func fnSize(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
	switch a[0].Kind() {
	case qv.KindString:
		s, _ := a[0].AsString()
		return qv.Int(int64(utf8.RuneCountInString(s))), nil
	case qv.KindBytes, qv.KindList:
		return qv.Int(int64(a[0].Len())), nil
	case qv.KindPath:
		return qv.Int(int64(len(a[0].Path().Edges))), nil
	}
	return qv.Value{}, mismatch(f, e, a[0])
}

// fnToInteger truncates a Float toward zero and parses a decimal String
// (sign, digits, '_' between digits); an unparsable String, a Bool and
// other kinds give Null. A Float that is not finite or does not fit an
// Int is INVALID_ARGUMENT.
func fnToInteger(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
	switch a[0].Kind() {
	case qv.KindInt:
		return a[0], nil
	case qv.KindFloat:
		x, _ := a[0].AsFloat()
		t := math.Trunc(x)
		if math.IsNaN(t) || t < -(1<<63) || t >= 1<<63 {
			return qv.Value{}, f.fail(e.Span, errs.CodeInvalidArgument, "the float does not convert to an integer")
		}
		return qv.Int(int64(t)), nil
	case qv.KindString:
		s, _ := a[0].AsString()
		digits, ok := underscores(s)
		if !ok {
			return qv.Value{}, nil
		}
		i, err := strconv.ParseInt(digits, 10, 64)
		if err != nil {
			return qv.Value{}, nil
		}
		return qv.Int(i), nil
	}
	return qv.Value{}, nil
}

// underscores removes the '_' separators of a number, which must stand
// between two digits (hexadecimal digits after a 0x prefix); ok is false
// when one does not.
func underscores(s string) (string, bool) {
	if !strings.Contains(s, "_") {
		return s, true
	}
	body := strings.TrimLeft(s, "+-")
	hex := len(body) > 1 && body[0] == '0' && body[1]|0x20 == 'x'
	digit := func(i int) bool {
		if i < 0 || i >= len(s) {
			return false
		}
		c := s[i]
		return '0' <= c && c <= '9' || hex && 'a' <= c|0x20 && c|0x20 <= 'f'
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '_' {
			if !digit(i-1) || !digit(i+1) {
				return "", false
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String(), true
}

// fnToFloat converts an Int to the nearest Float and parses a String as a
// float: decimal or hexadecimal, '_' between digits, and Inf, Infinity
// and NaN in any case; a value beyond the range is an infinity. An
// unparsable String, a Bool and other kinds give Null.
func fnToFloat(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
	switch a[0].Kind() {
	case qv.KindFloat:
		return a[0], nil
	case qv.KindInt:
		i, _ := a[0].AsInt()
		return qv.Float(float64(i)), nil
	case qv.KindString:
		s, _ := a[0].AsString()
		digits, ok := underscores(s)
		if !ok {
			return qv.Value{}, nil
		}
		x, err := strconv.ParseFloat(digits, 64)
		if err != nil {
			if ne, ok := err.(*strconv.NumError); !ok || ne.Err != strconv.ErrRange {
				return qv.Value{}, nil
			}
		}
		return qv.Float(x), nil
	}
	return qv.Value{}, nil
}

// fnToString formats Int, Float, Bool and String; a Float in plain
// decimal notation with the shortest digits that read back, integral
// floats without a fraction, and nan, inf, -inf. Other kinds give Null.
func fnToString(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
	var s string
	switch a[0].Kind() {
	case qv.KindString:
		return a[0], nil
	case qv.KindInt:
		i, _ := a[0].AsInt()
		s = strconv.FormatInt(i, 10)
	case qv.KindBool:
		b, _ := a[0].Scalar().AsBool()
		s = strconv.FormatBool(b)
	case qv.KindFloat:
		x, _ := a[0].AsFloat()
		switch {
		case math.IsNaN(x):
			s = "nan"
		case math.IsInf(x, 1):
			s = "inf"
		case math.IsInf(x, -1):
			s = "-inf"
		default:
			s = strconv.FormatFloat(x, 'f', -1, 64)
		}
	default:
		return qv.Value{}, nil
	}
	if err := f.grow(int64(len(s))); err != nil {
		return qv.Value{}, err
	}
	return qv.String(s), nil
}

func str(f *frame, e *ast.FuncCall, v qv.Value) (string, error) {
	s, ok := v.AsString()
	if !ok {
		return "", mismatch(f, e, v)
	}
	return s, nil
}

// fnCase maps ASCII letters only, byte by byte: other code points are
// kept, as in the reference fixtures.
func fnCase(upper bool) func(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
	return func(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
		s, err := str(f, e, a[0])
		if err != nil {
			return qv.Value{}, err
		}
		if err := f.grow(int64(len(s))); err != nil {
			return qv.Value{}, err
		}
		b := []byte(s)
		for i, c := range b {
			switch {
			case upper && 'a' <= c && c <= 'z':
				b[i] = c - 'a' + 'A'
			case !upper && 'A' <= c && c <= 'Z':
				b[i] = c - 'A' + 'a'
			}
		}
		return qv.String(string(b)), nil
	}
}

// fnTrim strips spaces, tabs, line feeds and carriage returns from both
// ends.
func fnTrim(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
	s, err := str(f, e, a[0])
	if err != nil {
		return qv.Value{}, err
	}
	return qv.String(strings.Trim(s, " \t\n\r")), nil
}

// position converts an Int, or a finite Float truncated toward zero, to
// a count of code points, saturating at the bounds of an int.
func position(f *frame, e *ast.FuncCall, v qv.Value) (int64, error) {
	if i, ok := v.AsInt(); ok {
		return i, nil
	}
	x, ok := v.AsFloat()
	if !ok {
		return 0, mismatch(f, e, v)
	}
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0, f.fail(e.Span, errs.CodeInvalidArgument, "%s needs a finite number", e.Name)
	}
	t := math.Trunc(x)
	switch {
	case t >= 1<<63:
		return math.MaxInt64, nil
	case t < -(1 << 63):
		return math.MinInt64, nil
	}
	return int64(t), nil
}

// fnSubstring takes code points from start, 0-based; a start beyond the
// string gives "", a negative start counts from 0, and a negative length
// gives "".
func fnSubstring(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
	s, err := str(f, e, a[0])
	if err != nil {
		return qv.Value{}, err
	}
	start, err := position(f, e, a[1])
	if err != nil {
		return qv.Value{}, err
	}
	length := int64(math.MaxInt64)
	if len(a) == 3 {
		if length, err = position(f, e, a[2]); err != nil {
			return qv.Value{}, err
		}
	}
	start, length = max(start, 0), max(length, 0)
	i, n := 0, int64(0)
	for ; n < start && i < len(s); n++ {
		_, w := utf8.DecodeRuneInString(s[i:])
		i += w
	}
	j := i
	for n = 0; n < length && j < len(s); n++ {
		_, w := utf8.DecodeRuneInString(s[j:])
		j += w
	}
	return qv.String(s[i:j]), nil
}

// fnReplace replaces every occurrence of search, left to right and
// without overlap; an empty search leaves the string unchanged. The size
// of the result is charged before it is built.
func fnReplace(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
	var s [3]string
	for i := range s {
		var err error
		if s[i], err = str(f, e, a[i]); err != nil {
			return qv.Value{}, err
		}
	}
	if s[1] == "" {
		return a[0], nil
	}
	n := int64(strings.Count(s[0], s[1]))
	size := int64(len(s[0])) + n*(int64(len(s[2]))-int64(len(s[1])))
	if err := f.grow(size); err != nil {
		return qv.Value{}, err
	}
	return qv.String(strings.ReplaceAll(s[0], s[1], s[2])), nil
}

// fnSplit keeps empty fields; an empty delimiter splits into code points.
func fnSplit(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
	s, err := str(f, e, a[0])
	if err != nil {
		return qv.Value{}, err
	}
	d, err := str(f, e, a[1])
	if err != nil {
		return qv.Value{}, err
	}
	n := int64(strings.Count(s, d)) + 1 // for d == "": code points + 1
	if d == "" {
		n--
	}
	if err := f.build(n, 2, qv.ElementBytes*(n+1)+int64(len(s))); err != nil {
		return qv.Value{}, err
	}
	parts := strings.Split(s, d)
	items := make([]qv.Value, len(parts))
	for i, p := range parts {
		items[i] = qv.String(p)
	}
	return qv.List(items), nil
}

func list(f *frame, e *ast.FuncCall, v qv.Value) ([]qv.Value, error) {
	if v.Kind() != qv.KindList {
		return nil, mismatch(f, e, v)
	}
	return v.Items(), nil
}

func fnHead(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
	items, err := list(f, e, a[0])
	if err != nil || len(items) == 0 {
		return qv.Value{}, err
	}
	return items[0], nil
}

func fnLast(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
	items, err := list(f, e, a[0])
	if err != nil || len(items) == 0 {
		return qv.Value{}, err
	}
	return items[len(items)-1], nil
}

func fnTail(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
	items, err := list(f, e, a[0])
	if err != nil {
		return qv.Value{}, err
	}
	if len(items) == 0 {
		return f.list(nil)
	}
	head := items[0]
	if err := f.build(a[0].Elements()-1-head.Elements(), a[0].Depth(), a[0].Size()-head.Size()); err != nil {
		return qv.Value{}, err
	}
	return qv.List(slices.Clone(items[1:])), nil
}

// fnRange is the inclusive range from start to end by step (default 1);
// a step of 0 is INVALID_ARGUMENT, and a range with more elements than
// MaxValueElements is RESOURCE_LIMIT before anything is allocated.
func fnRange(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
	var b [3]int64
	b[2] = 1
	for i := range a {
		var err error
		if b[i], err = position(f, e, a[i]); err != nil {
			return qv.Value{}, err
		}
	}
	start, end, step := b[0], b[1], b[2]
	if step == 0 {
		return qv.Value{}, f.fail(e.Span, errs.CodeInvalidArgument, "range needs a step other than 0")
	}
	// The count, computed in uint64 so that no step overflows.
	var n uint64
	switch {
	case step > 0 && start <= end:
		n = (uint64(end)-uint64(start))/uint64(step) + 1
	case step < 0 && start >= end:
		n = (uint64(start)-uint64(end))/(uint64(-(step+1))+1) + 1
	}
	count := int64(min(n, math.MaxInt64))
	if err := f.build(count, 2, 0); err != nil { // the shape first: no overflow below
		return qv.Value{}, err
	}
	if err := f.grow(qv.ElementBytes * (count + 1)); err != nil {
		return qv.Value{}, err
	}
	items := make([]qv.Value, count)
	for i := range items {
		items[i] = qv.Int(start + int64(i)*step) // within [start, end]
	}
	return qv.List(items), nil
}

func fnType(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
	if a[0].Kind() != qv.KindEdge {
		return qv.Value{}, mismatch(f, e, a[0])
	}
	t, err := f.env.Graph.EdgeType(f.ctx(), a[0].ID())
	if err != nil {
		return qv.Value{}, f.at(e.Span, err)
	}
	return qv.String(t), nil
}

func fnLabels(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
	if a[0].Kind() != qv.KindNode {
		return qv.Value{}, mismatch(f, e, a[0])
	}
	labels, err := f.env.Graph.Labels(f.ctx(), a[0].ID())
	if err != nil {
		return qv.Value{}, f.at(e.Span, err)
	}
	bytes := int64(qv.ElementBytes * (len(labels) + 1))
	for _, l := range labels {
		bytes += int64(len(l))
	}
	if err := f.build(int64(len(labels)), 2, bytes); err != nil {
		return qv.Value{}, err
	}
	items := make([]qv.Value, len(labels))
	for i, l := range labels {
		items[i] = qv.String(l)
	}
	return qv.List(items), nil
}

// fnProperties returns the properties of a node or an edge as a Map,
// sorted by key.
func fnProperties(f *frame, e *ast.FuncCall, a []qv.Value) (qv.Value, error) {
	switch a[0].Kind() {
	case qv.KindNode, qv.KindEdge:
	default:
		return qv.Value{}, mismatch(f, e, a[0])
	}
	props, err := f.env.Graph.Properties(f.ctx(), a[0].Kind(), a[0].ID())
	if err != nil {
		return qv.Value{}, f.at(e.Span, err)
	}
	pv := value.MapOf(props)
	if err := f.build(qv.ShapeOf(pv)); err != nil {
		return qv.Value{}, err
	}
	return qv.FromValue(pv), nil
}
