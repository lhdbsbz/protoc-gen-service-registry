// protoc-gen-service-registry 是「单进程微服务」脚手架的接线生成器，约定优先、零配置：
//
//   - 对每个 proto 服务生成 gen/local_service_center/<svc>.go（进程内 gRPC 注册 + 客户端获取）
//   - 聚合生成 gen/wiring/wiring_gen.go（BootAll + RegisterGateways），main.go / router.go 永不改动
//
// 一切路径从 proto 的 go_package 推导：go_package 必须形如 <module>/gen/proto/<pkg>，
// 实现包约定为 <module>/microservice/<snake(服务名)>_service，导出 var Service 和 func Main(ctx)。
// 唯一可选参数 impl_overrides 处理不符合蛇形约定的历史目录名。
package main

import (
	"bytes"
	"fmt"
	"go/format"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"text/template"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// ---- 约定（脚手架的法律，不做成参数）----

// 所有生成物统一落在 gen/ 下；gen/ 必须随时可整体删除并由 proto-gen 完整重建。
const (
	genProtoSeg   = "/gen/proto/"              // go_package 必须包含的段，用于推导 module 根
	registryDir   = "gen/local_service_center" // 每服务注册文件输出目录
	wiringOut     = "gen/wiring/wiring_gen.go" // 聚合接线文件
	wiringPackage = "wiring"
	implDir       = "microservice" // 实现包根：<module>/microservice/<seg>
	implSuffix    = "_service"
)

// 每服务注册文件模板（内嵌，gofmt 兜底格式化）
const registryTemplate = `package local_service_center

import (
	"context"
	"net"

	"{{.ProtoImportPath}}"
	"{{.ModuleRoot}}/pkg/local_service_center/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Register{{.ServiceName}}Service 注册{{.ServiceName}}服务
func Register{{.ServiceName}}Service(ctx context.Context, service {{.ProtoPackageName}}.{{.ProtoServiceName}}ServiceServer) {
	serviceInfo := {{.ProtoPackageName}}.{{.ProtoServiceName}}Service_ServiceDesc

	// 检查服务是否已注册
	if _, ok := core.GlobalRegistry.Discover(serviceInfo); ok {
		panic("服务重复注册: " + string(serviceInfo.ServiceName))
	}

	// 监听随机端口（同步进行，避免注册时序竞态）
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		panic("监听端口失败: " + err.Error())
	}

	// 注册服务地址（同步进行，确保调用端能立即 Discover 到）
	core.GlobalRegistry.RegisterAddr(serviceInfo, lis.Addr().String())

	go func() {
		// 创建gRPC服务器
		server := grpc.NewServer(
			grpc.ChainUnaryInterceptor(core.MetadataCalleeInterceptor),
			grpc.ChainStreamInterceptor(core.MetadataStreamCalleeInterceptor),
		)
		go func() {
			<-ctx.Done()
			server.GracefulStop()
		}()
		// 注册服务
		{{.ProtoPackageName}}.Register{{.ProtoServiceName}}ServiceServer(server, service)

		// 启动服务器
		if err = server.Serve(lis); err != nil {
			panic("服务启动失败: " + err.Error())
		}
	}()
}

// Get{{.ServiceName}}Service 获取{{.ServiceName}}服务客户端
func Get{{.ServiceName}}Service() {{.ProtoPackageName}}.{{.ProtoServiceName}}ServiceClient {
	serviceInfo := {{.ProtoPackageName}}.{{.ProtoServiceName}}Service_ServiceDesc

	// 尝试获取已缓存的客户端
	if client, exists := core.GlobalRegistry.GetClient(serviceInfo); exists {
		return client.({{.ProtoPackageName}}.{{.ProtoServiceName}}ServiceClient)
	}

	// 发现服务地址
	addr := core.GlobalRegistry.MustDiscover(serviceInfo)

	// 创建连接
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(core.MetadataCallerInterceptor),
		grpc.WithChainStreamInterceptor(core.MetadataStreamCallerInterceptor),
	)
	if err != nil {
		panic("创建客户端连接失败: " + err.Error())
	}

	// 创建客户端
	client := {{.ProtoPackageName}}.New{{.ProtoServiceName}}ServiceClient(conn)

	// 缓存客户端
	core.GlobalRegistry.RegisterClient(serviceInfo, client)

	return client
}

// Get{{.ServiceName}}ServiceAddressInfo 获取{{.ServiceName}}服务的具体地址
func Get{{.ServiceName}}ServiceAddressInfo() string {
	serviceInfo := {{.ProtoPackageName}}.{{.ProtoServiceName}}Service_ServiceDesc

	// 尝试获取已注册的地址
	if addr, exists := core.GlobalRegistry.Discover(serviceInfo); exists {
		return addr
	}
	return ""
}
`

type registryData struct {
	ServiceName      string // Go注册函数名称前缀，如 OpenApiMemory
	ProtoServiceName string // Proto服务名称前缀，如 Memory (如果是 MemoryService)
	ProtoPackageName string // proto 生成包名，如 user
	ProtoImportPath  string // proto 生成包导入路径
	ModuleRoot       string // 从 go_package 推导的 module 根
}

type wsMethod struct {
	Name           string // 方法名，如 Transcribe
	RequestType    string // 请求类型，如 AudioChunk
	ResponseType   string // 响应类型，如 Transcript
	ReqBytesPaths  string // Go [][]string 字面量，空则 "nil"
	RespBytesPaths string // Go [][]string 字面量，空则 "nil"
}

// wiringService 聚合接线所需的单个服务信息。
type wiringService struct {
	FullName     string     // 完整服务名，如 UserService（用于网关注册函数名）
	ProtoPkg     string     // proto 包名，如 user
	ProtoImport  string     // proto 生成包导入路径
	ImplSeg      string     // 实现包目录段，如 user_service（同时用作 import 别名）
	ImplImport   string     // 实现包完整导入路径
	HasGateway   bool       // 是否有带 google.api.http 注解的方法，或者包含 WebSocket 流式方法
	Category     string     // "openapi" 或 "grpc-gateway"
	WsMethods    []wsMethod // WebSocket 流式方法列表
	HasHttpRoute bool       // 是否具有 http 路由方法（包含 google.api.http），以此决定是否生成 RegisterXxxHandlerFromEndpoint
	// 走 grpc-gateway mux 的纯 server-streaming 方法全名（/pkg.Service/Method）：响应须 SSE 直出、不套信封。
	StreamingHttpMethods []string
}

func main() {
	tmpl := template.Must(template.New("registry").Parse(registryTemplate))

	protogen.Options{}.Run(func(gen *protogen.Plugin) error {
		overrides, err := parseOverrides(pluginParameter(gen))
		if err != nil {
			return err
		}

		moduleRoot := ""
		var wiring []wiringService
		for _, f := range gen.Files {
			if !f.Generate {
				continue
			}
			if len(f.Services) == 0 {
				continue
			}

			root, err := deriveModuleRoot(string(f.GoImportPath))
			if err != nil {
				return err
			}
			if moduleRoot == "" {
				moduleRoot = root
			} else if moduleRoot != root {
				return fmt.Errorf("service-registry: go_package 推导出不一致的 module 根: %q vs %q", moduleRoot, root)
			}

			for _, svc := range f.Services {
				if err := generateServiceRegistry(gen, f, svc, tmpl, root); err != nil {
					return err
				}
				wiring = append(wiring, buildWiringService(f, svc, root, overrides))
			}
		}

		if len(wiring) == 0 {
			return nil
		}
		return generateWiring(gen, wiring)
	})
}

func pluginParameter(gen *protogen.Plugin) string {
	if gen.Request.Parameter != nil {
		return *gen.Request.Parameter
	}
	return ""
}

// parseOverrides 解析唯一参数 impl_overrides=Stem:seg;Stem2:seg2（可省略）。
func parseOverrides(param string) (map[string]string, error) {
	overrides := map[string]string{}
	for _, pair := range strings.Split(param, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(k) != "impl_overrides" {
			return nil, fmt.Errorf("service-registry: unknown option %q (only impl_overrides is supported)", pair)
		}
		for _, entry := range strings.Split(v, ";") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			stem, seg, ok := strings.Cut(entry, ":")
			if !ok || strings.TrimSpace(stem) == "" || strings.TrimSpace(seg) == "" {
				return nil, fmt.Errorf("service-registry: invalid impl_overrides entry %q (want Stem:seg)", entry)
			}
			overrides[strings.TrimSpace(stem)] = strings.TrimSpace(seg)
		}
	}
	return overrides, nil
}

// deriveModuleRoot 从 go_package（如 github.com/x/backend/gen/proto/user）推导 module 根。
func deriveModuleRoot(goImportPath string) (string, error) {
	idx := strings.Index(goImportPath, genProtoSeg)
	if idx <= 0 {
		return "", fmt.Errorf("service-registry: go_package %q 不符合约定 <module>%s<pkg>", goImportPath, genProtoSeg)
	}
	return goImportPath[:idx], nil
}

func generateServiceRegistry(gen *protogen.Plugin, file *protogen.File, service *protogen.Service, tmpl *template.Template, moduleRoot string) error {
	protoServiceName := strings.TrimSuffix(string(service.Desc.Name()), "Service")

	protoRelPath := strings.TrimPrefix(string(file.GoImportPath), moduleRoot + genProtoSeg)
	registryName := deriveRegistryName(protoRelPath, protoServiceName)

	data := registryData{
		ServiceName:      registryName,
		ProtoServiceName: protoServiceName,
		ProtoPackageName: string(file.GoPackageName),
		ProtoImportPath:  string(file.GoImportPath),
		ModuleRoot:       moduleRoot,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return fmt.Errorf("service-registry: execute template: %w", err)
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("service-registry: format %s: %w", registryName, err)
	}

	rel := filepath.Join(registryDir, fmt.Sprintf("%s.go", lowerFirst(registryName)))
	g := gen.NewGeneratedFile(rel, "")
	if _, err := g.Write(formatted); err != nil {
		return fmt.Errorf("service-registry: write %s: %w", rel, err)
	}
	return nil
}

// buildWiringService 从 proto 服务推导聚合接线所需信息。
func buildWiringService(file *protogen.File, service *protogen.Service, moduleRoot string, overrides map[string]string) wiringService {
	full := string(service.Desc.Name())         // UserService
	stem := strings.TrimSuffix(full, "Service") // User

	protoRelPath := strings.TrimPrefix(string(file.GoImportPath), moduleRoot + genProtoSeg)
	seg := overrides[stem]
	if seg == "" {
		seg = strings.ReplaceAll(protoRelPath, "/", "_") + implSuffix
	}
	parts := strings.Split(protoRelPath, "/")
	var implPath string
	if len(parts) > 1 {
		implPath = strings.Join(parts[:len(parts)-1], "/") + "/" + parts[len(parts)-1]
	} else {
		implPath = protoRelPath
	}

	var wsMethods []wsMethod
	var streamingHttp []string
	pkgName := string(file.Desc.Package()) // proto 包名，如 proto.story
	for _, method := range service.Methods {
		// 如果是客户端流式（Client-streaming）或者双向流式（Bidi-streaming），加入 WS 映射列表
		if method.Desc.IsStreamingClient() {
			wsMethods = append(wsMethods, wsMethod{
				Name:           string(method.Desc.Name()),
				RequestType:    string(method.Input.Desc.Name()),
				ResponseType:   string(method.Output.Desc.Name()),
				ReqBytesPaths:  goBytesPathsLiteral(collectBytesPaths(method.Input.Desc)),
				RespBytesPaths: goBytesPathsLiteral(collectBytesPaths(method.Output.Desc)),
			})
		}
		// 纯 server-streaming 且挂了 google.api.http：走 grpc-gateway mux、按 SSE 直出，
		// 须在响应改写处跳过统一信封。全名格式与运行时 runtime.RPCMethod 一致：/pkg.Service/Method。
		if methodHasHTTP(method) && method.Desc.IsStreamingServer() && !method.Desc.IsStreamingClient() {
			streamingHttp = append(streamingHttp,
				fmt.Sprintf("/%s.%s/%s", pkgName, full, string(method.Desc.Name())))
		}
	}

	hasGateway := serviceHasHTTP(service) || len(wsMethods) > 0

	return wiringService{
		FullName:             full,
		ProtoPkg:             strings.ReplaceAll(protoRelPath, "/", "_"),
		ProtoImport:          string(file.GoImportPath),
		ImplSeg:              seg,
		ImplImport:           moduleRoot + "/" + implDir + "/" + implPath,
		HasGateway:           hasGateway,
		Category:             serviceGatewayCategory(service),
		WsMethods:            wsMethods,
		HasHttpRoute:         serviceHasHTTP(service),
		StreamingHttpMethods: streamingHttp,
	}
}

func serviceGatewayCategory(service *protogen.Service) string {
	for _, method := range service.Methods {
		options, ok := method.Desc.Options().(*descriptorpb.MethodOptions)
		if !ok || options == nil {
			continue
		}
		ext := proto.GetExtension(options, annotations.E_Http)
		if ext == nil {
			continue
		}
		rule, ok := ext.(*annotations.HttpRule)
		if !ok || rule == nil {
			continue
		}
		v := reflect.ValueOf(rule).Elem().FieldByName("Pattern")
		if !v.IsValid() || v.IsNil() {
			continue
		}
		patternInterface := v.Interface()
		var path string
		switch pat := patternInterface.(type) {
		case *annotations.HttpRule_Post:
			path = pat.Post
		case *annotations.HttpRule_Get:
			path = pat.Get
		case *annotations.HttpRule_Put:
			path = pat.Put
		case *annotations.HttpRule_Delete:
			path = pat.Delete
		case *annotations.HttpRule_Patch:
			path = pat.Patch
		}
		if strings.HasPrefix(path, "/openapi/") {
			return "openapi"
		}
		if strings.HasPrefix(path, "/grpc-gateway/") {
			return "grpc-gateway"
		}
	}
	return "grpc-gateway"
}

// generateWiring 生成单份聚合接线文件：BootAll + RegisterGateways + MuxRegistry + StreamingGatewayMethods。
func generateWiring(gen *protogen.Plugin, svcs []wiringService) error {
	// 收集并排序 import（顺序不影响编译，仅为稳定可读）。
	type imp struct{ alias, path string }
	implSet := map[string]string{}  // alias -> path（实现包，全部服务都要）
	protoSet := map[string]string{} // alias -> path（proto 包，仅网关服务）
	for _, s := range svcs {
		implSet[s.ImplSeg] = s.ImplImport
		if s.HasGateway {
			protoSet[s.ProtoPkg] = s.ProtoImport
		}
	}
	var imports []imp
	for a, p := range implSet {
		imports = append(imports, imp{a, p})
	}
	for a, p := range protoSet {
		imports = append(imports, imp{a, p})
	}
	sort.Slice(imports, func(i, j int) bool { return imports[i].path < imports[j].path })

	var buf bytes.Buffer
	buf.WriteString("// Code generated by protoc-gen-service-registry. DO NOT EDIT.\n\n")
	fmt.Fprintf(&buf, "package %s\n\n", wiringPackage)
	buf.WriteString("import (\n")
	buf.WriteString("\t\"context\"\n\n")
	buf.WriteString("\t\"github.com/grpc-ecosystem/grpc-gateway/v2/runtime\"\n")
	buf.WriteString("\t\"google.golang.org/grpc\"\n")
	buf.WriteString("\t\"google.golang.org/grpc/credentials/insecure\"\n")
	buf.WriteString("\t\"google.golang.org/protobuf/proto\"\n\n")
	buf.WriteString("\t\"github.com/lhdbsbz/backend/gen/local_service_center\"\n")
	buf.WriteString("\t\"github.com/lhdbsbz/backend/pkg/grpc_gateway_util\"\n")
	buf.WriteString("\t\"github.com/lhdbsbz/backend/pkg/local_service_center/core\"\n\n")
	for _, im := range imports {
		fmt.Fprintf(&buf, "\t%s %q\n", im.alias, im.path)
	}
	buf.WriteString(")\n\n")

	// BootAll：启动全部进程内服务（顺序 = proto 遍历顺序；客户端惰性连接，顺序无关）。
	buf.WriteString("// BootAll 启动所有进程内服务。新增服务无需改此文件，重新生成即可。\n")
	buf.WriteString("func BootAll(ctx context.Context) {\n")
	for _, s := range svcs {
		fmt.Fprintf(&buf, "\t%s.Main(ctx)\n", s.ImplSeg)
	}
	buf.WriteString("}\n\n")

	// RegisterGateways：注册所有带 HTTP 网关的服务到 grpc-gateway mux。
	buf.WriteString("// RegisterGateways 注册所有带 HTTP 网关的服务到 grpc-gateway mux。\n")
	buf.WriteString("func RegisterGateways(ctx context.Context, mux *runtime.ServeMux) {\n")
	buf.WriteString("\topts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}\n")
	for _, s := range svcs {
		if !s.HasHttpRoute {
			continue
		}
		fmt.Fprintf(&buf, "\t_ = %s.Register%sHandlerFromEndpoint(ctx, mux,\n", s.ProtoPkg, s.FullName)
		fmt.Fprintf(&buf, "\t\tcore.GlobalRegistry.MustDiscover(%s.%s_ServiceDesc), opts)\n", s.ProtoPkg, s.FullName)
	}
	buf.WriteString("}\n\n")

	// MuxRegistry：方法路径 → 拨流+类型工厂+bytes路径。供单 mux 端点 (MuxServe) 按 OPEN 帧查表。
	buf.WriteString("\n// MuxRegistry 是单 WS 多路复用端点的方法注册表：OPEN 帧的方法路径映射到拨流与编解码信息。\n")
	buf.WriteString("var MuxRegistry = map[string]grpc_gateway_util.MuxEntry{\n")
	for _, s := range svcs {
		for _, m := range s.WsMethods {
			fmt.Fprintf(&buf, "\t\"/%s/%s\": {\n", s.FullName, m.Name)
			fmt.Fprintf(&buf, "\t\tDial: func(ctx context.Context) (grpc.ClientStream, error) { return local_service_center.Get%s().%s(ctx) },\n", s.FullName, m.Name)
			fmt.Fprintf(&buf, "\t\tNewReq:  func() proto.Message { return &%s.%s{} },\n", s.ProtoPkg, m.RequestType)
			fmt.Fprintf(&buf, "\t\tNewResp: func() proto.Message { return &%s.%s{} },\n", s.ProtoPkg, m.ResponseType)
			fmt.Fprintf(&buf, "\t\tReqBytesPaths:  %s,\n", m.ReqBytesPaths)
			fmt.Fprintf(&buf, "\t\tRespBytesPaths: %s,\n", m.RespBytesPaths)
			fmt.Fprintf(&buf, "\t},\n")
		}
	}
	buf.WriteString("}\n")

	// StreamingGatewayMethods：走 grpc-gateway mux 的 server-streaming 方法全名集合。
	// 响应须按 SSE 逐块直出、不套统一信封 {code,message,data}。由「server-streaming + google.api.http」
	// 自动判定，新增此类接口无需手改 pkg/grpc_gateway_util——重新生成即可，调用方按需引用本集合。
	var streamingMethods []string
	for _, s := range svcs {
		streamingMethods = append(streamingMethods, s.StreamingHttpMethods...)
	}
	sort.Strings(streamingMethods)
	buf.WriteString("\n// StreamingGatewayMethods 是走 grpc-gateway mux 的 server-streaming 方法全名集合，\n")
	buf.WriteString("// 响应按 SSE 逐块直出、不套统一信封。由 server-streaming + google.api.http 自动判定。\n")
	buf.WriteString("var StreamingGatewayMethods = map[string]bool{\n")
	for _, m := range streamingMethods {
		fmt.Fprintf(&buf, "\t%q: true,\n", m)
	}
	buf.WriteString("}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("service-registry: format wiring: %w\n---\n%s", err, buf.String())
	}
	g := gen.NewGeneratedFile(wiringOut, "")
	if _, err := g.Write(formatted); err != nil {
		return fmt.Errorf("service-registry: write %s: %w", wiringOut, err)
	}
	return nil
}

// collectBytesPaths 遍历消息描述符，算出所有 bytes 字段的 JSON key 路径（protojson 用 JSONName=lowerCamel）。
// 递归进入 message 字段；以"当前在栈上"的 FullName 集合阻断自引用环，但允许同一类型在不同分支重复出现（菱形）。
// 从 protoc-gen-frontend-api 复制，供生成 MuxRegistry 的 ReqBytesPaths/RespBytesPaths 使用。
func collectBytesPaths(msg protoreflect.MessageDescriptor) [][]string {
	var out [][]string
	onStack := map[protoreflect.FullName]bool{}

	var walk func(m protoreflect.MessageDescriptor, prefix []string)
	walk = func(m protoreflect.MessageDescriptor, prefix []string) {
		if onStack[m.FullName()] {
			return
		}
		onStack[m.FullName()] = true
		defer delete(onStack, m.FullName())

		fields := m.Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			path := append(append([]string{}, prefix...), f.JSONName())

			if f.IsMap() {
				if f.MapValue().Kind() == protoreflect.BytesKind {
					out = append(out, path)
				}
				continue
			}

			switch f.Kind() {
			case protoreflect.BytesKind:
				out = append(out, path)
			case protoreflect.MessageKind, protoreflect.GroupKind:
				walk(f.Message(), path)
			}
		}
	}
	walk(msg, nil)
	return out
}

// goBytesPathsLiteral 把 [][]string 序列化为 Go 复合字面量；空返回 "nil"。
func goBytesPathsLiteral(paths [][]string) string {
	if len(paths) == 0 {
		return "nil"
	}
	var b strings.Builder
	b.WriteString("[][]string{")
	for i, p := range paths {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("{")
		for j, seg := range p {
			if j > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%q", seg)
		}
		b.WriteString("}")
	}
	b.WriteString("}")
	return b.String()
}

// serviceHasHTTP 判断服务是否有带 google.api.http 注解的方法（即是否需要网关注册）。
func serviceHasHTTP(service *protogen.Service) bool {
	for _, method := range service.Methods {
		if methodHasHTTP(method) {
			return true
		}
	}
	return false
}

// methodHasHTTP 返回该方法是否带 google.api.http 注解。
func methodHasHTTP(method *protogen.Method) bool {
	options, ok := method.Desc.Options().(*descriptorpb.MethodOptions)
	if !ok || options == nil {
		return false
	}
	ext := proto.GetExtension(options, annotations.E_Http)
	if ext == nil {
		return false
	}
	rule, ok := ext.(*annotations.HttpRule)
	if !ok || rule == nil {
		return false
	}
	// 用反射安全访问 Pattern（与 protoc-gen-frontend-api 一致）。
	v := reflect.ValueOf(rule).Elem().FieldByName("Pattern")
	return v.IsValid() && !v.IsNil()
}

// lowerFirst 首字母转小写（注册文件名：User -> user.go, IdGenerator -> idGenerator.go）。
func lowerFirst(s string) string {
	if len(s) == 0 {
		return s
	}
	if c := s[0]; c >= 'A' && c <= 'Z' {
		return string(c+32) + s[1:]
	}
	return s
}

// toCamelCase 将 snake_case 或 kebab-case 转换为 CamelCase (e.g. open_api -> OpenApi, memory -> Memory)
func toCamelCase(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-'
	})
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
		}
	}
	return strings.Join(parts, "")
}

func deriveRegistryName(protoRelPath string, protoServiceName string) string {
	parts := strings.Split(protoRelPath, "/")
	if len(parts) > 1 {
		var prefixParts []string
		for _, p := range parts[:len(parts)-1] {
			prefixParts = append(prefixParts, toCamelCase(p))
		}
		return strings.Join(prefixParts, "") + protoServiceName
	}
	return protoServiceName
}

