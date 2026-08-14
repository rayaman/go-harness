package luatool

import (
	"encoding/json"
	"fmt"
	"sync"

	lua "github.com/yuin/gopher-lua"
)

// LuaTool wraps a gopher-lua VM state that defines a tool in Lua script.
type LuaTool struct {
	mu     sync.Mutex
	L      *lua.LState
	script string
}

// NewLuaToolFromScript initializes a new tool from a Lua script string.
func NewLuaToolFromScript(script string) (*LuaTool, error) {
	L := lua.NewState()

	// Enable the safe HTTP module
	RegisterHTTPModule(L, DefaultHTTPConfig)

	if err := L.DoString(script); err != nil {
		L.Close()
		return nil, fmt.Errorf("failed to parse lua tool script: %w", err)
	}

	tool := &LuaTool{
		L:      L,
		script: script,
	}

	// Verify required Lua functions exist
	for _, fn := range []string{"name", "description", "parameters", "execute"} {
		if L.GetGlobal(fn).Type() != lua.LTFunction {
			L.Close()
			return nil, fmt.Errorf("lua tool missing required function: %s()", fn)
		}
	}

	return tool, nil
}

func (t *LuaTool) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.L != nil {
		t.L.Close()
	}
}

func (t *LuaTool) Name() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	err := t.L.CallByParam(lua.P{
		Fn:      t.L.GetGlobal("name"),
		NRet:    1,
		Protect: true,
	})
	if err != nil {
		return ""
	}
	ret := t.L.Get(-1)
	t.L.Pop(1)
	return ret.String()
}

func (t *LuaTool) Description() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	err := t.L.CallByParam(lua.P{
		Fn:      t.L.GetGlobal("description"),
		NRet:    1,
		Protect: true,
	})
	if err != nil {
		return ""
	}
	ret := t.L.Get(-1)
	t.L.Pop(1)
	return ret.String()
}

func (t *LuaTool) Parameters() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()

	err := t.L.CallByParam(lua.P{
		Fn:      t.L.GetGlobal("parameters"),
		NRet:    1,
		Protect: true,
	})
	if err != nil {
		return map[string]any{}
	}
	ret := t.L.Get(-1)
	t.L.Pop(1)

	if m, ok := luaToGo(ret).(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func (t *LuaTool) Execute(args json.RawMessage) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var goArgs map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &goArgs); err != nil {
			return "", fmt.Errorf("invalid json arguments: %w", err)
		}
	}

	luaArgs := goToLua(t.L, goArgs)

	// Call execute(args) in Lua. Expects (result_str, err_str_or_nil)
	err := t.L.CallByParam(lua.P{
		Fn:      t.L.GetGlobal("execute"),
		NRet:    2,
		Protect: true,
	}, luaArgs)

	if err != nil {
		return "", fmt.Errorf("lua runtime error: %w", err)
	}

	luaErr := t.L.Get(-1)
	luaRes := t.L.Get(-2)
	t.L.Pop(2)

	if luaErr != lua.LNil {
		return "", fmt.Errorf("tool error: %s", luaErr.String())
	}

	return luaRes.String(), nil
}

// -----------------------------------------------------------------------------
// Lua <-> Go Type Conversion Helpers
// -----------------------------------------------------------------------------

func goToLua(L *lua.LState, val any) lua.LValue {
	switch v := val.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(v)
	case string:
		return lua.LString(v)
	case float64:
		return lua.LNumber(v)
	case int:
		return lua.LNumber(v)
	case map[string]any:
		tbl := L.NewTable()
		for k, item := range v {
			tbl.RawSetString(k, goToLua(L, item))
		}
		return tbl
	case []any:
		tbl := L.NewTable()
		for _, item := range v {
			tbl.Append(goToLua(L, item))
		}
		return tbl
	default:
		return lua.LString(fmt.Sprintf("%v", v))
	}
}

func luaToGo(val lua.LValue) any {
	switch v := val.(type) {
	case lua.LBool:
		return bool(v)
	case lua.LNumber:
		return float64(v)
	case lua.LString:
		return string(v)
	case *lua.LTable:
		// Determine if table is array-like or map-like
		isArr := true
		maxIdx := 0
		v.ForEach(func(key, _ lua.LValue) {
			if num, ok := key.(lua.LNumber); ok && num > 0 {
				if int(num) > maxIdx {
					maxIdx = int(num)
				}
			} else {
				isArr = false
			}
		})

		if isArr && maxIdx > 0 {
			arr := make([]any, maxIdx)
			v.ForEach(func(key, val lua.LValue) {
				idx := int(key.(lua.LNumber)) - 1
				arr[idx] = luaToGo(val)
			})
			return arr
		}

		m := make(map[string]any)
		v.ForEach(func(key, val lua.LValue) {
			m[key.String()] = luaToGo(val)
		})
		return m
	default:
		return nil
	}
}
