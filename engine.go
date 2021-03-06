package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nzlov/goreq"
	"github.com/tidwall/gjson"
	"github.com/yuin/gopher-lua"

	luajson "layeh.com/gopher-json"
	"layeh.com/gopher-luar"
)

type cmderr struct {
	name string
	err  error
}

type Engine struct {
	commands   map[string]*Command
	cmdmap     map[string]*Command
	noP        map[string]*Command
	wait       *sync.WaitGroup
	cmdNum     int64
	cmdRunNum  int64
	cmdSuccess int64
	cmdFailed  int64
	cmdResult  chan *cmderr
	context    *Context
}

func NewEngine(path string) *Engine {
	if path == "" {
		panic(fmt.Errorf("config not fount"))
	}
	body, err := ioutil.ReadFile(path)
	if err != nil {
		panic(err)
	}
	context := NewContext()
	err = json.Unmarshal(body, context)
	if err != nil {
		panic(err)
	}

	return &Engine{
		commands:  map[string]*Command{},
		cmdmap:    map[string]*Command{},
		noP:       map[string]*Command{},
		wait:      &sync.WaitGroup{},
		cmdResult: make(chan *cmderr, 10),
		context:   context,
	}
}

func (e *Engine) Load(paths ...string) {
	for _, p := range paths {
		e.load(p)
	}
}

func (e *Engine) load(path string) {
	log.Println("Engine Load:", path)
	body, err := ioutil.ReadFile(path)
	if err != nil {
		panic(err)
	}
	//	log.Println("Engine Load:read:", string(body))
	commands, err := commandsFromJSON(body)
	if err != nil {
		panic(err)
	}
	//	log.Printf("Engine Load Commands:%+v\n", commands)
	for _, c := range commands {
		if c.Name == "" {
			panic(fmt.Errorf("json config error:has empty name!"))
		}
		if _, has := e.commands[c.Name]; has {
			panic(fmt.Errorf("Load %s Error:Command %s exited.", path, c.Name))
		}
		if _, has := e.cmdmap[c.Name]; has {
			panic(fmt.Errorf("Load %s Error:Command %s exited.", path, c.Name))
		}
		e.cmdmap[c.Name] = c
		if c.Require == "" {
			e.commands[c.Name] = c
		} else {
			e.noP[c.Name] = c
		}
		for _, sc := range c.SubCommand {
			if _, has := e.cmdmap[sc.Name]; has {
				panic(fmt.Errorf("Load %s Error:Command %s exited.", path, sc.Name))
			}
			sc.Require = ""
			e.cmdmap[sc.Name] = sc
		}
	}

}

func (e *Engine) Start() {
	log.Printf("Engine Check Commands:%+v\n", e.cmdmap)
	lnoP := len(e.noP)

	for lnoP != 0 {
		for _, c := range e.noP {
			//log.Printf("noP:%+v\n", c)
			if r, ok := e.cmdmap[c.Require]; ok {
				r.SubCommand = append(r.SubCommand, c)
				delete(e.noP, c.Name)
				//	log.Printf("noP Delete:%+v\n", c)
			}
		}

		if len(e.noP) == lnoP {
			panic(fmt.Errorf("Commands [%+v] don't find Require.\n", e.noP))
		}
		lnoP = len(e.noP)
	}
	e.cmdNum = int64(len(e.cmdmap))

	for k := range e.cmdmap {
		delete(e.cmdmap, k)
	}

	log.Printf("Engine Start %d Commands:%+v\n", e.cmdNum, e.commands)
	e.wait.Add(len(e.commands))
	for _, c := range e.commands {
		go func(cmd *Command) {
			defer e.wait.Done()
			context := NewContextWithCopy(e.context)
			err := e.Exec(nil, context, cmd)
			if err != nil {
				e.cmdResult <- &cmderr{
					name: cmd.Name,
					err:  err,
				}
				atomic.AddInt64(&e.cmdFailed, 1)
			}
		}(c)
	}
	errs := map[string]error{}
	over := make(chan struct{})
	go func() {
		for ce := range e.cmdResult {
			log.Printf("%+v\n", ce)
			errs[ce.name] = ce.err
		}
		over <- struct{}{}
	}()
	e.wait.Wait()
	close(e.cmdResult)
	<-over
	log.Println("Start Over.")
	log.Printf("Start %d commands,Exec %d commands.Success %d,Failed %d\n", e.cmdNum, e.cmdRunNum, e.cmdSuccess, e.cmdFailed)
	if e.cmdFailed > 0 {
		log.Println("Faileds:")
		for k, v := range errs {
			log.Printf("Command %s has Error:%v\n", k, v)

		}
	}
}

func (e *Engine) Exec(req *goreq.GoReq, context *Context, cmd *Command) error {
	log.Printf("Engine Exec:%+v\n", cmd)
	atomic.AddInt64(&e.cmdRunNum, 1)
	if req == nil {
		req = goreq.New()
		req.Debug = true
	}

	switch cmd.Method {
	case "POST", "post", "p", "P":
		req.Post(context.P(cmd.URL))
	case "DELETE", "delete", "d", "D":
		req.Delete(context.P(cmd.URL))
	default:
		req.Get(context.P(cmd.URL))
	}

	for k, v := range cmd.Header {
		req.SetHeader(context.P(k), context.P(v))
	}
	if len(context.Header) > 0 {
		for k, v := range context.Header {
			req.SetHeader(k, v)
		}
	}

	if cmd.ContentType != "" {
		req.ContentType(cmd.ContentType)
	}
	if cmd.URLParams != nil {
		paramstr := context.P(string(*cmd.URLParams))
		req.Query(paramstr)
	}
	if cmd.Params != nil {
		paramstr := context.P(string(*cmd.Params))
		req.SendMapString(paramstr)
	}

	if len(cmd.RequestLua) > 0 {
		l := lua.NewState()
		l.SetGlobal("Context", luar.New(l, context))
		l.SetGlobal("Cmd", luar.New(l, cmd))
		l.SetGlobal("Req", luar.New(l, req))
		for _, path := range cmd.RequestLua {
			err := l.DoFile(path)
			if err != nil {
				l.Close()
				return err
			}
		}
		l.Close()
	}

	_, body, errs := req.EndBytes()
	if len(errs) != 0 {
		return errs[0]
	}

	// resp := map[string]interface{}{}
	// err := json.Unmarshal(body, &resp)
	// if err != nil {
	// 	log.Println("Engine Exec Resp Error:", string(body), err)
	// 	panic(err)
	// }

	gjsons := gjson.ParseBytes(body)

	//log.Printf("Engine Exec Resp:%+v\n", string(body))

	for k, v := range cmd.Return {
		kp := context.P(k)
		if vp, ok := v.(string); ok {
			v = context.P(vp)
		}
		rv := gjsons.Get(kp)
		if !rv.Exists() {
			return fmt.Errorf("Resp key %s[%s] don't exists.", k, kp)
		}
		rvi := rv.Value()
		if reflect.TypeOf(v).Name() == reflect.TypeOf(rvi).Name() && fmt.Sprint(v) == fmt.Sprint(rvi) {
			continue
		}
		return fmt.Errorf("Key:%s[%s] Value:%v[%s] != %v[%s]", k, kp, rvi, reflect.TypeOf(rvi).Name(), v, reflect.TypeOf(v).Name())
	}

	for _, v := range cmd.ReturnLua {
		l := lua.NewState()
		luajson.Preload(l)
		err := l.DoFile(v)
		if err != nil {
			l.Close()
			return err
		}
		if err = l.CallByParam(lua.P{
			Fn:      l.GetGlobal("check"),
			NRet:    2,
			Protect: true,
		}, lua.LString(string(body))); err != nil {
			return err
		}
		ret := l.Get(1)
		retStr := l.Get(2)
		l.Pop(2)
		if b, ok := ret.(lua.LBool); ok {
			if !b {
				if s, ok := retStr.(lua.LString); ok {
					return fmt.Errorf(string(s))
				} else {
					return fmt.Errorf("RETURNLUA [%s] return args two don't is string:%v", v, retStr)
				}
			}
		} else {
			return fmt.Errorf("RETURNLUA [%s] return args one don't is bool:%v", v, ret)
		}

		l.Close()
	}

	for k, v := range cmd.Context {
		kp := context.P(k)
		v = context.P(v)
		rv := gjsons.Get(kp)
		if !rv.Exists() {
			return fmt.Errorf("Resp key %s[%s] don't exists.", k, kp)
		}
		vs := strings.Split(v, "|")
		var value interface{}
		if len(vs) == 2 {
			switch vs[1] {
			case "int":
				switch rv.Type {
				case gjson.Number:
					value = int64(rv.Num)
				case gjson.False:
					value = int64(0)
				case gjson.True:
					value = int64(1)
				default:
					i, err := strconv.ParseInt(fmt.Sprintf("%v", rv.Value()), 10, 64)
					if err != nil {
						return err
					}
					value = i
				}
			case "float":
				f, err := strconv.ParseFloat(fmt.Sprintf("%v", rv.Value()), 64)
				if err != nil {
					return err
				}
				value = f
			case "string":
				value = fmt.Sprint(rv.Value())
			}
		} else {
			value = rv.Value()
		}

		context.K(vs[0], value)
	}

	if len(cmd.NextLua) > 0 {
		l := lua.NewState()
		l.SetGlobal("Context", luar.New(l, context))
		l.SetGlobal("Cmd", luar.New(l, cmd))
		for _, path := range cmd.NextLua {
			err := l.DoFile(path)
			if err != nil {
				l.Close()
				return err
			}
		}
		l.Close()
	}

	for _, c := range cmd.SubCommand {
		e.wait.Add(1)
		go func(req *goreq.GoReq, cmd *Command) {
			defer e.wait.Done()
			ncontext := NewContextWithCopy(context)
			err := e.Exec(req, ncontext, cmd)
			if err != nil {
				e.cmdResult <- &cmderr{
					name: cmd.Name,
					err:  err,
				}
				atomic.AddInt64(&e.cmdFailed, 1)
			}
		}(goreq.NewWithGoReq(req), c)
	}
	atomic.AddInt64(&e.cmdSuccess, 1)
	return nil
}
