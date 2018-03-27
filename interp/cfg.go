package interp

type Symbol struct {
	typ   *Type // type of value
	index int   // index of value in frame
}

type Scope struct {
	anc   *Scope             // Ancestor scope
	level int                // Frame level: number of frame indirections to access var
	sym   map[string]*Symbol // Symbol table indexed by idents
}

func (s *Scope) push(indirect int) *Scope {
	return &Scope{anc: s, level: s.level + indirect, sym: make(map[string]*Symbol)}
}

func (s *Scope) lookup(ident string) (*Symbol, int, bool) {
	level := s.level
	for s != nil {
		if sym, ok := s.sym[ident]; ok {
			return sym, level - s.level, true
		}
		s = s.anc
	}
	return nil, 0, false
}

// n.Cfg() generates a control flow graph (CFG) from AST (wiring successors in AST)
// and pre-compute frame sizes and indexes for all un-named (temporary) and named
// variables.
// Following this pass, the CFG is ready to run
func (e *Node) Cfg(tdef TypeDef, sdef SymDef) int {
	scope := &Scope{sym: make(map[string]*Symbol)}
	maxIndex := 0
	var loop, loopRestart *Node

	e.Walk(func(n *Node) bool {
		// Pre-order processing
		switch n.kind {
		case Define, AssignStmt:
			if l := len(n.Child); l%2 == 1 {
				// Odd number of children: remove the type node, useless for assign
				i := l / 2
				n.Child = append(n.Child[:i], n.Child[i+1:]...)
			}

		case For0, ForRangeStmt:
			loop, loopRestart = n, n.Child[0]

		case For1, For2, For3, For4:
			loop, loopRestart = n, n.Child[len(n.Child)-1]

		case FuncDecl:
			maxIndex = 0
			scope = scope.push(1)
			if len(n.Child[0].Child) > 0 {
				// function is a method, add it to the related type
				n.ident = n.Child[1].ident
				tname := n.Child[0].Child[0].Child[1].ident
				t := tdef[tname]
				t.method = append(t.method, n)
			}
			// allocate entries for return values at start of frame
			if len(n.Child[2].Child) == 2 {
				maxIndex += len(n.Child[2].Child[1].Child)
			}

		case Switch0:
			// Make sure default clause is in last position
			c := n.Child[1].Child
			if i, l := getDefault(n), len(c)-1; i >= 0 && i != l {
				c[i], c[l] = c[l], c[i]
			}

		case TypeSpec:
			// Type analysis is performed recursively and no post-order processing
			// needs to be done for types, so do not dive in subtree
			if n.Child[1].kind == Ident {
				// Create a type alias of an existing one
				tdef[n.Child[0].ident] = &Type{cat: AliasT, val: nodeType(tdef, n.Child[1])}
			} else {
				// Define a new type
				tdef[n.Child[0].ident] = nodeType(tdef, n.Child[1])
			}
			return false

		case ArrayType, ChanType, MapType, StructType:
			n.typ = nodeType(tdef, n)
			return false

		case BasicLit:
			switch n.val.(type) {
			case bool:
				n.typ = tdef["bool"]
			case float64:
				n.typ = tdef["float64"]
			case int:
				n.typ = tdef["int"]
			case string:
				n.typ = tdef["string"]
			}
		}
		return true
	}, func(n *Node) {
		// Post-order processing
		switch n.kind {
		case ArrayType:
			// TODO: move to pre-processing ? See when handling complex array type def
			n.typ = &Type{cat: ArrayT, val: tdef[n.Child[1].ident]}

		case Define, AssignStmt:
			wireChild(n)
			n.findex = n.Child[0].findex
			// Propagate type
			// TODO: Check that existing destination type matches source type
			n.Child[0].typ = n.Child[1].typ
			n.typ = n.Child[0].typ
			if sym, _, ok := scope.lookup(n.Child[0].ident); ok {
				sym.typ = n.typ
			}
			// If LHS is an indirection, get reference instead of value, to allow setting
			if n.Child[0].action == GetIndex {
				if n.Child[0].Child[0].typ.cat == MapT {
					n.Child[0].run = getMap
					n.run = assignMap
				} else {
					n.Child[0].run = getIndexAddr
					n.run = assignField
				}
			}

		case IncDecStmt:
			wireChild(n)
			n.findex = n.Child[0].findex
			n.Child[0].typ = tdef["int"]
			n.typ = n.Child[0].typ
			//if sym, ok := symbol[n.Child[0].ident]; ok {
			if sym, _, ok := scope.lookup(n.Child[0].ident); ok {
				sym.typ = n.typ
			}

		case DefineX, AssignXStmt:
			wireChild(n)
			n.findex = n.Child[0].findex
			n.Child[0].typ = n.Child[1].typ
			n.typ = n.Child[0].typ

		case BinaryExpr:
			wireChild(n)
			maxIndex++
			n.findex = maxIndex
			n.typ = n.Child[0].typ

		case IndexExpr:
			wireChild(n)
			maxIndex++
			n.findex = maxIndex
			n.typ = n.Child[0].typ
			if n.typ.cat == MapT {
				n.run = getIndexMap
			}

		case BlockStmt, DeclStmt, ExprStmt, GenDecl, ParenExpr, SendStmt:
			wireChild(n)
			n.findex = n.Child[len(n.Child)-1].findex

		case Break:
			n.tnext = loop

		case CallExpr:
			wireChild(n)
			maxIndex++
			n.findex = maxIndex
			if builtin, ok := goBuiltin[n.Child[0].ident]; ok {
				n.run = builtin
				if n.Child[0].ident == "make" {
					if n.typ = tdef[n.Child[1].ident]; n.typ != nil {
						n.Child[1].val = n.typ
						n.Child[1].kind = BasicLit
					} else {
						n.typ = nodeType(tdef, n.Child[1])
						n.Child[1].val = n.typ
						n.Child[1].kind = BasicLit
					}
				}
			}
			if n.Child[0].kind == SelectorExpr {
				// Resolve method and receiver path, store them in node static value for run
				n.val, n.Child[0].Child[1].val = n.Child[0].Child[0].typ.lookupMethod(n.Child[0].Child[1].ident)
				n.fsize = len(n.val.(*Node).Child[2].Child[1].Child)
			} else {
				n.val = sdef[n.Child[0].ident]
				if def := n.val.(*Node); def != nil {
					// Reserve as many frame entries as nb of ret values for called function
					// node frame index should point to the first entry
					j := len(def.Child[2].Child) - 1
					l := len(def.Child[2].Child[j].Child) // Number of return values for def
					maxIndex += l - 1
					if l == 1 {
						// If def returns exactly one value, propagate its type in call node.
						// Multiple return values will be handled differently through AssignX.
						n.typ = tdef[def.Child[2].Child[j].Child[0].Child[0].ident]
					}
					n.fsize = l
				}
			}
			//fmt.Println(n.index, "callExpr:", n.Child[0].ident, "frame index:", n.findex)

		case CaseClause:
			maxIndex++
			n.findex = maxIndex
			n.tnext = n.Child[len(n.Child)-1].Start

		case CompositeLitExpr:
			wireChild(n)
			maxIndex++
			n.findex = maxIndex
			if n.Child[0].typ == nil {
				n.Child[0].typ = tdef[n.Child[0].ident]
			}
			// TODO: Check that composite litteral expr matches corresponding type
			n.typ = n.Child[0].typ
			switch n.typ.cat {
			case ArrayT:
				n.run = arrayLit
			case MapT:
				n.run = mapLit
			case StructT:
				n.action, n.run = CompositeLit, compositeLit
				// Handle object assign from sparse key / values
				if len(n.Child) > 1 && n.Child[1].kind == KeyValueExpr {
					n.run = compositeSparse
					n.typ = tdef[n.Child[0].ident]
					for _, c := range n.Child[1:] {
						c.findex = n.typ.fieldIndex(c.Child[0].ident)
					}
				}
			}

		case Continue:
			n.tnext = loopRestart

		case Field:
			// A single child node (no ident, just type) means that the field refers
			// to a return value, and space on frame should be accordingly allocated.
			// Otherwise, just point to corresponding location in frame, resolved in
			// ident child.
			if len(n.Child) == 1 {
				maxIndex++
				n.findex = maxIndex
			} else {
				l := len(n.Child) - 1
				t := tdef[n.Child[l].ident]
				for _, f := range n.Child[:l] {
					//symbol[f.ident].typ = t
					scope.sym[f.ident].typ = t
				}
			}

		case File:
			wireChild(n)
			n.fsize = maxIndex + 2

		case For0: // for {}
			body := n.Child[0]
			n.Start = body.Start
			body.tnext = n.Start
			loop, loopRestart = nil, nil

		case For1: // for cond {}
			cond, body := n.Child[0], n.Child[1]
			n.Start = cond.Start
			cond.tnext = body.Start
			cond.fnext = n
			body.tnext = cond.Start
			loop, loopRestart = nil, nil

		case For2: // for init; cond; {}
			init, cond, body := n.Child[0], n.Child[1], n.Child[2]
			n.Start = init.Start
			init.tnext = cond.Start
			cond.tnext = body.Start
			cond.fnext = n
			body.tnext = cond.Start
			loop, loopRestart = nil, nil

		case For3: // for ; cond; post {}
			cond, post, body := n.Child[0], n.Child[1], n.Child[2]
			n.Start = cond.Start
			cond.tnext = body.Start
			cond.fnext = n
			body.tnext = post.Start
			post.tnext = cond.Start
			loop, loopRestart = nil, nil

		case For4: // for init; cond; post {}
			init, cond, post, body := n.Child[0], n.Child[1], n.Child[2], n.Child[3]
			n.Start = init.Start
			init.tnext = cond.Start
			cond.tnext = body.Start
			cond.fnext = n
			body.tnext = post.Start
			post.tnext = cond.Start
			loop, loopRestart = nil, nil

		case ForRangeStmt:
			loop, loopRestart = nil, nil
			n.Start = n.Child[0].Start
			n.findex = n.Child[0].findex

		case FuncDecl:
			n.fsize = maxIndex + 1 // Why ????
			if len(n.Child[0].Child) > 0 {
				// Store receiver frame location (used at run)
				n.Child[0].findex = n.Child[0].Child[0].Child[0].findex
			}
			scope = scope.anc

		case FuncType:
			n.typ = nodeType(tdef, n)
			// Store list of parameter frame indices in params val
			var list []int
			for _, c := range n.Child[0].Child {
				for _, f := range c.Child[:len(c.Child)-1] {
					list = append(list, f.findex)
				}
			}
			n.Child[0].val = list
			// TODO: do the same for return values

		case GoStmt:
			wireChild(n)
			// TODO: should error if call expression refers to a builtin
			n.Child[0].run = callGoRoutine

		case Ident:
			// Lookup identifier in frame symbol table. If not found
			// should check if ident can be defined (assign, param passing...)
			// or should lookup in upper scope of variables
			// For now, simply allocate a new entry in local sym table
			if sym, level, ok := scope.lookup(n.ident); ok {
				n.typ, n.findex, n.level = sym.typ, sym.index, level

			} else {
				maxIndex++
				scope.sym[n.ident] = &Symbol{index: maxIndex}
				n.findex = maxIndex
			}

		case If0: // if cond {}
			cond, tbody := n.Child[0], n.Child[1]
			n.Start = cond.Start
			cond.tnext = tbody.Start
			cond.fnext = n
			tbody.tnext = n

		case If1: // if cond {} else {}
			cond, tbody, fbody := n.Child[0], n.Child[1], n.Child[2]
			n.Start = cond.Start
			cond.tnext = tbody.Start
			cond.fnext = fbody.Start
			tbody.tnext = n
			fbody.tnext = n

		case If2: // if init; cond {}
			init, cond, tbody := n.Child[0], n.Child[1], n.Child[2]
			n.Start = init.Start
			tbody.tnext = n
			init.tnext = cond.Start
			cond.tnext = tbody.Start
			cond.fnext = n

		case If3: // if init; cond {} else {}
			init, cond, tbody, fbody := n.Child[0], n.Child[1], n.Child[2], n.Child[3]
			n.Start = init.Start
			init.tnext = cond.Start
			cond.tnext = tbody.Start
			cond.fnext = fbody.Start
			tbody.tnext = n
			fbody.tnext = n

		case KeyValueExpr:
			wireChild(n)

		case LandExpr:
			n.Start = n.Child[0].Start
			n.Child[0].tnext = n.Child[1].Start
			n.Child[0].fnext = n
			n.Child[1].tnext = n
			maxIndex++
			n.findex = maxIndex
			n.typ = n.Child[0].typ

		case LorExpr:
			n.Start = n.Child[0].Start
			n.Child[0].tnext = n
			n.Child[0].fnext = n.Child[1].Start
			n.Child[1].tnext = n
			maxIndex++
			n.findex = maxIndex
			n.typ = n.Child[0].typ

		case RangeStmt:
			n.Start = n
			n.Child[3].tnext = n
			n.tnext = n.Child[3].Start
			maxIndex++
			n.findex = maxIndex

		case ReturnStmt:
			wireChild(n)
			n.tnext = nil

		case SelectorExpr:
			wireChild(n)
			maxIndex++
			n.findex = maxIndex
			n.typ = n.Child[0].typ
			// lookup field index once during compiling (simple and fast first)
			if fi := n.typ.fieldIndex(n.Child[1].ident); fi >= 0 {
				n.typ = n.typ.field[fi].typ
				n.Child[1].kind = BasicLit
				n.Child[1].val = fi
			} else {
				// Handle promoted field in embedded struct
				if ti := n.typ.lookupField(n.Child[1].ident); len(ti) > 0 {
					n.Child[1].kind = BasicLit
					n.Child[1].val = ti
					n.run = getIndexSeq
				} else {
					//fmt.Println(n.index, "Selector not found:", n.Child[1].ident)
					n.run = nop
					//panic("Field not found in selector")
				}
			}

		case Switch0:
			n.Start = n.Child[1].Start
			// Chain case clauses
			clauses := n.Child[1].Child
			l := len(clauses)
			for i, c := range clauses[:l-1] {
				// chain to next clause
				c.tnext = c.Child[1].Start
				c.Child[1].tnext = n
				c.fnext = clauses[i+1]
			}
			// Handle last clause
			if c := clauses[l-1]; len(c.Child) > 1 {
				// No default clause
				c.tnext = c.Child[1].Start
				c.fnext = n
				c.Child[1].tnext = n
			} else {
				// Default
				c.tnext = c.Child[0].Start
				c.Child[0].tnext = n
			}

		case ValueSpec:
			l := len(n.Child) - 1
			n.typ = tdef[n.Child[l].ident]
			for _, c := range n.Child[:l] {
				c.typ = n.typ
			}
		}
	})
	return maxIndex + 1
}

// find default case clause index of a switch statement, if any
func getDefault(n *Node) int {
	for i, c := range n.Child[1].Child {
		if len(c.Child) == 1 {
			return i
		}
	}
	return -1
}

// Wire AST nodes for CFG in subtree
func wireChild(n *Node) {
	// Set start node, in subtree (propagated to ancestors by post-order processing)
	for _, child := range n.Child {
		switch child.kind {
		case ArrayType, ChanType, FuncDecl, MapType, BasicLit, Ident:
			continue
		default:
			n.Start = child.Start
		}
		break
	}

	// Chain sequential operations inside a block (next is right sibling)
	for i := 1; i < len(n.Child); i++ {
		n.Child[i-1].tnext = n.Child[i].Start
	}

	// Chain subtree next to self
	for i := len(n.Child) - 1; i >= 0; i-- {
		switch n.Child[i].kind {
		case ArrayType, ChanType, MapType, FuncDecl, BasicLit, Ident:
			continue
		case Break, Continue, ReturnStmt:
			// tnext is already computed, no change
		default:
			n.Child[i].tnext = n
		}
		break
	}
}
