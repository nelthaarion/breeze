package breeze

// maxRouteOrder is larger than any registration index.
const maxRouteOrder = int(^uint(0) >> 1)

// segNodeLinear is how many static children a node finds by scanning its keys
// before it also builds a map: small fan-outs are cheaper to scan than to hash.
const segNodeLinear = 8

// segNode is a node of a method bucket's segment trie.
//
// A pattern is a path of segments. A literal segment descends into the static
// child of that name, a :param segment into the node's single param child (the
// parameter names are irrelevant to matching, they only matter when the params
// map is built from the winning route), and a trailing *wildcard marks the node
// the route ends on. The route is stored on that node as exact (it matches only
// when the request ends there) or wild (it matches when the request ends there or
// continues past it).
//
// min is the lowest registration index of any route in the subtree. match uses
// it to skip subtrees that cannot beat a route it already holds, which is what
// lets a depth-first walk return the first-registered match without visiting
// every candidate: the result is identical to scanning the routes in order.
type segNode struct {
	keys  []string
	kids  []*segNode
	index map[string]*segNode // mirrors keys/kids once len(keys) > segNodeLinear
	param *segNode
	exact *route
	wild  *route
	min   int
}

func newSegNode() *segNode { return &segNode{min: maxRouteOrder} }

func (n *segNode) child(seg string) *segNode {
	if n.index != nil {
		return n.index[seg]
	}
	for i, k := range n.keys {
		if k == seg {
			return n.kids[i]
		}
	}
	return nil
}

func (n *segNode) staticChild(seg string) *segNode {
	if c := n.child(seg); c != nil {
		return c
	}
	c := newSegNode()
	n.keys = append(n.keys, seg)
	n.kids = append(n.kids, c)
	switch {
	case n.index != nil:
		n.index[seg] = c
	case len(n.keys) > segNodeLinear:
		n.index = make(map[string]*segNode, 2*len(n.keys))
		for i, k := range n.keys {
			n.index[k] = n.kids[i]
		}
	}
	return c
}

func (n *segNode) paramChild() *segNode {
	if n.param == nil {
		n.param = newSegNode()
	}
	return n.param
}

// insertTrie adds rt to the bucket's trie. Routes arrive in registration order,
// so rt.order only grows and the first route to reach a node sets its min.
func (idx *methodIndex) insertTrie(rt *route) {
	if idx.root == nil {
		idx.root = newSegNode()
	}
	cur := idx.root
	if rt.order < cur.min {
		cur.min = rt.order
	}
	for i, seg := range rt.segments {
		if rt.paramIndex[i] {
			cur = cur.paramChild()
		} else {
			cur = cur.staticChild(seg)
		}
		if rt.order < cur.min {
			cur.min = rt.order
		}
	}
	// A later route of the same shape ends on the same node and matches exactly
	// the requests the earlier one matches, so it could never win: keep the first.
	if rt.hasWildcard {
		if cur.wild == nil {
			cur.wild = rt
		}
	} else if cur.exact == nil {
		cur.exact = rt
	}
}

// match walks segs[depth:] below n and returns the matching route with the lowest
// registration index, or best when nothing below n beats it.
func (n *segNode) match(segs []string, depth int, best *route, bestIdx int) (*route, int) {
	if n.min >= bestIdx {
		return best, bestIdx
	}
	// Reaching n means depth request segments matched, which is all a wildcard
	// anchored here needs: it matches the remaining segments, however many.
	if w := n.wild; w != nil && w.order < bestIdx {
		best, bestIdx = w, w.order
	}
	if depth == len(segs) {
		if e := n.exact; e != nil && e.order < bestIdx {
			best, bestIdx = e, e.order
		}
		return best, bestIdx
	}
	if c := n.child(segs[depth]); c != nil {
		best, bestIdx = c.match(segs, depth+1, best, bestIdx)
	}
	if n.param != nil {
		best, bestIdx = n.param.match(segs, depth+1, best, bestIdx)
	}
	return best, bestIdx
}

// scan is the ordered scan: the first route of the bucket with this method that
// matches. bucketOther mixes methods and is small, so it keeps this.
func (idx *methodIndex) scan(method Method, segs []string) *route {
	for _, rt := range idx.routes {
		if rt.method == method && rt.matchesSegments(segs) {
			return rt
		}
	}
	return nil
}

// bindParams builds the params map for the winning route. The map is nil for a
// route with neither parameters nor a wildcard.
func bindParams(rt *route, path string, segs []string) map[string]string {
	if !rt.hasWildcard && rt.paramCount == 0 {
		return nil
	}
	params := acquireParams()
	for i, rseg := range rt.segments {
		if rt.paramIndex[i] {
			params[rseg[1:]] = segs[i]
		}
	}
	if rt.hasWildcard {
		params[rt.wildcardName] = pathTail(path, segs, len(rt.segments))
	}
	return params
}

// pathTail returns strings.Join(segs[k:], "/") without building a new string:
// segs are the '/'-separated pieces of path, so the join is a suffix of it.
func pathTail(path string, segs []string, k int) string {
	if k >= len(segs) {
		return ""
	}
	off := 0
	for i := 0; i < k; i++ {
		off += len(segs[i]) + 1
	}
	return path[off:]
}
