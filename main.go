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
	"unicode"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
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
func Register{{.ServiceName}}Service(ctx context.Context, service {{.ProtoPackageName}}.{{.ServiceName}}ServiceServer) {
	serviceInfo := {{.ProtoPackageName}}.{{.ServiceName}}Service_ServiceDesc

	// 检查服务是否已注册
	if _, ok := core.GlobalRegistry.Discover(serviceInfo); ok {
		panic("服务重复注册: " + string(serviceInfo.ServiceName))
	}

	go func() {
		// 监听随机端口
		lis, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			panic("监听端口失败: " + err.Error())
		}

		// 创建gRPC服务器
		server := grpc.NewServer(
			grpc.ChainUnaryInterceptor(core.MetadataCalleeInterceptor),
		)
		go func() {
			<-ctx.Done()
			server.GracefulStop()
		}()
		// 注册服务
		{{.ProtoPackageName}}.Register{{.ServiceName}}ServiceServer(server, service)

		// 注册服务地址
		core.GlobalRegistry.RegisterAddr(serviceInfo, lis.Addr().String())

		// 启动服务器
		if err = server.Serve(lis); err != nil {
			panic("服务启动失败: " + err.Error())
		}
	}()
}

// Get{{.ServiceName}}Service 获取{{.ServiceName}}服务客户端
func Get{{.ServiceName}}Service() {{.ProtoPackageName}}.{{.ServiceName}}ServiceClient {
	serviceInfo := {{.ProtoPackageName}}.{{.ServiceName}}Service_ServiceDesc

	// 尝试获取已缓存的客户端
	if client, exists := core.GlobalRegistry.GetClient(serviceInfo); exists {
		return client.({{.ProtoPackageName}}.{{.ServiceName}}ServiceClient)
	}

	// 发现服务地址
	addr := core.GlobalRegistry.MustDiscover(serviceInfo)

	// 创建连接
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(core.MetadataCallerInterceptor),
	)
	if err != nil {
		panic("创建客户端连接失败: " + err.Error())
	}

	// 创建客户端
	client := {{.ProtoPackageName}}.New{{.ServiceName}}ServiceClient(conn)

	// 缓存客户端
	core.GlobalRegistry.RegisterClient(serviceInfo, client)

	return client
}

// Get{{.ServiceName}}ServiceAddressInfo 获取{{.ServiceName}}服务的具体地址
func Get{{.ServiceName}}ServiceAddressInfo() string {
	serviceInfo := {{.ProtoPackageName}}.{{.ServiceName}}Service_ServiceDesc

	// 尝试获取已注册的地址
	if addr, exists := core.GlobalRegistry.Discover(serviceInfo); exists {
		return addr
	}
	return ""
}
`

type registryData struct {
	ServiceName      string // 去掉 Service 后缀，如 User
	ProtoPackageName string // proto 生成包名，如 user
	ProtoImportPath  string // proto 生成包导入路径
	ModuleRoot       string // 从 go_package 推导的 module 根
}

// wiringService 聚合接线所需的单个服务信息。
type wiringService struct {
	FullName    string // 完整服务名，如 UserService（用于网关注册函数名）
	ProtoPkg    string // proto 包名，如 user
	ProtoImport string // proto 生成包导入路径
	ImplSeg     string // 实现包目录段，如 user_service（同时用作 import 别名）
	ImplImport  string // 实现包完整导入路径
	HasGateway  bool   // 是否有带 google.api.http 注解的方法
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
	serviceName := strings.TrimSuffix(string(service.Desc.Name()), "Service")

	data := registryData{
		ServiceName:      serviceName,
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
		return fmt.Errorf("service-registry: format %s: %w", serviceName, err)
	}

	rel := filepath.Join(registryDir, fmt.Sprintf("%s.go", lowerFirst(serviceName)))
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
	seg := overrides[stem]
	if seg == "" {
		seg = toSnakeCase(stem) + implSuffix
	}
	return wiringService{
		FullName:    full,
		ProtoPkg:    string(file.GoPackageName),
		ProtoImport: string(file.GoImportPath),
		ImplSeg:     seg,
		ImplImport:  moduleRoot + "/" + implDir + "/" + seg,
		HasGateway:  serviceHasHTTP(service),
	}
}

// generateWiring 生成单份聚合接线文件：BootAll + RegisterGateways。
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
	buf.WriteString("\t\"github.com/grpc-ecosystem/grpc-gateway/v2/runtime\"\n\n")
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

	// RegisterGateways：仅注册带 HTTP 注解的服务。
	buf.WriteString("// RegisterGateways 注册所有带 HTTP 网关的服务到 grpc-gateway mux。\n")
	buf.WriteString("func RegisterGateways(ctx context.Context, mux *runtime.ServeMux) {\n")
	for _, s := range svcs {
		if !s.HasGateway {
			continue
		}
		fmt.Fprintf(&buf, "\t_ = %s.Register%sHandlerServer(ctx, mux, %s.Service)\n", s.ProtoPkg, s.FullName, s.ImplSeg)
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

// toSnakeCase 驼峰转蛇形，能正确处理全大写缩写：
// User->user, IdGenerator->id_generator, TdxPlugin->tdx_plugin, MCP->mcp, AI->ai。
// 与实际目录不一致的历史命名（WebPage->webpage_service 等）用 impl_overrides 覆盖。
func toSnakeCase(s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		if unicode.IsUpper(r) {
			if i > 0 {
				prev := runes[i-1]
				next := rune(0)
				if i+1 < len(runes) {
					next = runes[i+1]
				}
				// 在「小写/数字 -> 大写」或「大写 -> 大写后接小写」的边界插入下划线
				if unicode.IsLower(prev) || unicode.IsDigit(prev) ||
					(unicode.IsUpper(prev) && next != 0 && unicode.IsLower(next)) {
					b.WriteByte('_')
				}
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
