package main

import (
	"bytes"
	"fmt"
	"github.com/golang/protobuf/proto"
	desc "github.com/golang/protobuf/protoc-gen-go/descriptor"
	ppb "github.com/golang/protobuf/protoc-gen-go/plugin"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const genDebug = false

func main() {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(os.Stdin)
	if err != nil {
		panic(fmt.Errorf("error reading from stdin: %v", err))
	}
	out, err := codeGenerator(buf.Bytes())
	if err != nil {
		panic(err)
	}
	os.Stdout.Write(out)
}

func codeGenerator(b []byte) ([]byte, error) {
	req := ppb.CodeGeneratorRequest{}
	err := proto.Unmarshal(b, &req)
	if err != nil {
		return nil, fmt.Errorf("error unmarshaling CodeGeneratorRequest: %v", err)
	}
	resp := gen(&req)
	out, err := proto.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("error marshaling CodeGeneratorResponse: %v", err)
	}
	return out, nil
}

func gen(req *ppb.CodeGeneratorRequest) *ppb.CodeGeneratorResponse {
	resp := &ppb.CodeGeneratorResponse{}
	fileToGenerate := map[string]bool{}
	for _, f := range req.FileToGenerate {
		fileToGenerate[f] = true
	}

	optGenService := false
	optLibraryImport := "protobuf"

	params := strings.Split(req.GetParameter(), ",")
	for _, param := range params {
		if param == "plugin=grpc" {
			optGenService = true
			continue
		}
		if strings.HasPrefix(param, "library_import=") {
			optLibraryImport = strings.TrimPrefix(param, "library_import=")
			continue
		}
		panic("unknown compiler option: " + param)
	}

	rootns := NewEmptyNamespace()
	for _, fdp := range req.ProtoFile {
		rootns.Parse(fdp)
		// panic(rootns.PrettyPrint()) // for debuggling

		if !fileToGenerate[fdp.GetName()] {
			continue
		}
		f := &ppb.CodeGeneratorResponse_File{}

		f.Name = proto.String(tsFileName(fdp) + ".ts")

		b := &bytes.Buffer{}
		w := &writer{b, 0}

		libMod := &modRef{
			alias: "__pb__",
			path:  optLibraryImport,
		}

		imports := writeFile(w, fdp, rootns, libMod, optGenService)
		beforeReplace := b.String()
		if strings.Contains(beforeReplace, "__long.") {
			imports = imports + "import * as __long from 'long'\n"
		}
		content := strings.Replace(beforeReplace, importPlaceholder, imports, 1)
		f.Content = proto.String(content)
		resp.File = append(resp.File, f)
	}
	return resp
}

func tsFileName(fdp *desc.FileDescriptorProto) string {
	fext := filepath.Ext(fdp.GetName())
	fname := strings.TrimSuffix(fdp.GetName(), fext)
	return fname + "_pb"
}

const importPlaceholder = "!!!IMPORT_PLACEHOLDER!!!"

func writeFile(w *writer, fdp *desc.FileDescriptorProto, rootNs *Namespace, libMod *modRef, genService bool) string {
	if fdp.GetSyntax() != "proto3" {
		panic(fmt.Errorf("unsupported syntax: %s in file %s", fdp.GetSyntax(), fdp.GetName()))
	}

	ns := rootNs.FindFullyQualifiedNamespace("." + fdp.GetPackage())
	mr := &moduleResolver{fdp, map[string]*modRef{}}
	if ns == nil {
		panic("unable to find namespace for: " + fdp.GetPackage())
	}
	w.p("// Generated by the protocol buffer compiler.  DO NOT EDIT!")
	w.p("// Source: %s", fdp.GetName())
	w.ln()
	w.p(importPlaceholder)
	w.ln()

	// Top level enums.
	for _, edp := range fdp.EnumType {
		writeEnum(w, edp, nil)
	}

	// Messages, recurse.
	for _, dp := range fdp.MessageType {
		writeDescriptor(w, dp, ns, mr, libMod, nil)
	}

	// Services
	if genService {
		for _, sdp := range fdp.Service {
			writeService(w, sdp, fdp.GetPackage(), ns, mr, libMod)
		}
	}

	imports := fmt.Sprintf("import * as %s from '%s'\n", libMod.alias, libMod.path)
	for _, mod := range mr.references {
		imports += fmt.Sprintf("import * as %s from '%s'\n", mod.alias, mod.path)
	}
	return imports
}

type modRef struct {
	alias, path string
}

type moduleResolver struct {
	currentFile *desc.FileDescriptorProto
	references  map[string]*modRef
}

func (m *moduleResolver) ToRelativeModule(fdp *desc.FileDescriptorProto) *modRef {
	if fdp.GetName() == m.currentFile.GetName() {
		return nil
	}
	mod := m.references[fdp.GetName()]
	if mod == nil {
		cwd := filepath.Dir(m.currentFile.GetName())
		path := tsFileName(fdp)
		path, _ = filepath.Rel(cwd, path)
		mod = &modRef{
			alias: "___" + strings.Replace(path, "/", "_", -1),
			path:  "./" + path,
		}
		m.references[fdp.GetName()] = mod
	}
	return mod
}

type oneof struct {
	odp         *desc.OneofDescriptorProto
	fields      []*field
	fqNamespace string
	typeName    string
}

type field struct {
	fd              *desc.FieldDescriptorProto
	typeTsName      string
	typeDescriptor  interface{}
	typeNs          *Namespace
	typeEnumDefault string
	isMap           bool
	oneof           *oneof
	typeFqProtoName string
	mr              *moduleResolver
}

func newField(fd *desc.FieldDescriptorProto, ns *Namespace, mr *moduleResolver) *field {
	f := &field{
		fd: fd,
		mr: mr,
	}
	if fd.GetTypeName() != "" {
		typeNs, typeName, i, typeFdp := ns.FindFullyQualifiedName(fd.GetTypeName())
		f.typeFqProtoName = typeNs + "." + typeName

		f.typeTsName = typeName
		if mod := mr.ToRelativeModule(typeFdp); mod != nil {
			f.typeTsName = mod.alias + "." + f.typeTsName
		}

		f.typeDescriptor = i
		f.typeNs = ns.FindFullyQualifiedNamespace(typeNs)
		if dp, ok := f.typeDescriptor.(*desc.DescriptorProto); ok {
			if dp.GetOptions().GetMapEntry() {
				f.isMap = true
			}
		}
		if ed, ok := f.typeDescriptor.(*desc.EnumDescriptorProto); ok {
			for _, v := range ed.Value {
				if v.GetNumber() == 0 {
					f.typeEnumDefault = v.GetName()
					break
				}
			}
		}
	}
	return f
}

func (f field) isOneofMember() bool {
	return f.fd.OneofIndex != nil
}

func (f field) varName() string {
	return f.fd.GetName()
}

func (f field) mapFields() (*field, *field) {
	dp := f.typeDescriptor.(*desc.DescriptorProto)
	keyField := newField(dp.Field[0], f.typeNs, f.mr)
	valueField := newField(dp.Field[1], f.typeNs, f.mr)
	return keyField, valueField
}

func (f field) tsType() string {
	switch t := *f.fd.Type; t {
	case desc.FieldDescriptorProto_TYPE_STRING:
		return "string"
	case desc.FieldDescriptorProto_TYPE_BYTES:
		return "Uint8Array"
	case desc.FieldDescriptorProto_TYPE_INT64,
		desc.FieldDescriptorProto_TYPE_UINT64,
		desc.FieldDescriptorProto_TYPE_SINT64,
		desc.FieldDescriptorProto_TYPE_FIXED64,
		desc.FieldDescriptorProto_TYPE_SFIXED64:
		return "__long"
	case desc.FieldDescriptorProto_TYPE_INT32,
		desc.FieldDescriptorProto_TYPE_UINT32,
		desc.FieldDescriptorProto_TYPE_SINT32,
		desc.FieldDescriptorProto_TYPE_FIXED32,
		desc.FieldDescriptorProto_TYPE_SFIXED32:
		return "number"
	case desc.FieldDescriptorProto_TYPE_FLOAT,
		desc.FieldDescriptorProto_TYPE_DOUBLE:
		return "number"
	case desc.FieldDescriptorProto_TYPE_BOOL:
		return "boolean"
	case desc.FieldDescriptorProto_TYPE_MESSAGE,
		desc.FieldDescriptorProto_TYPE_GROUP:
		return f.typeTsName
	case desc.FieldDescriptorProto_TYPE_ENUM:
		return f.typeTsName
	default:
		panic(fmt.Errorf("unexpected proto type while converting to php type: %v", t))
	}

}

func (f field) defaultValue() string {
	if f.isMap {
		return fmt.Sprintf("new %s()", f.labeledType())
	}
	if f.isRepeated() {
		return "[]"
	}
	switch t := *f.fd.Type; t {
	case desc.FieldDescriptorProto_TYPE_STRING:
		return `""`
	case desc.FieldDescriptorProto_TYPE_BYTES:
		return `new Uint8Array(0)`
	case desc.FieldDescriptorProto_TYPE_INT64,
		desc.FieldDescriptorProto_TYPE_SINT64,
		desc.FieldDescriptorProto_TYPE_SFIXED64:
		return "__long.ZERO"
	case desc.FieldDescriptorProto_TYPE_UINT64,
		desc.FieldDescriptorProto_TYPE_FIXED64:
		return "__long.UZERO"
	case desc.FieldDescriptorProto_TYPE_INT32,
		desc.FieldDescriptorProto_TYPE_UINT32,
		desc.FieldDescriptorProto_TYPE_SINT32,
		desc.FieldDescriptorProto_TYPE_FIXED32,
		desc.FieldDescriptorProto_TYPE_SFIXED32:
		return "0"
	case desc.FieldDescriptorProto_TYPE_FLOAT,
		desc.FieldDescriptorProto_TYPE_DOUBLE:
		return "0.0"
	case desc.FieldDescriptorProto_TYPE_BOOL:
		return "false"
	case desc.FieldDescriptorProto_TYPE_MESSAGE,
		desc.FieldDescriptorProto_TYPE_GROUP:
		return "null"
	case desc.FieldDescriptorProto_TYPE_ENUM:
		return "0"
	default:
		panic(fmt.Errorf("unexpected proto type while converting to php type: %v", t))
	}
}

func (f field) isPacked() bool {
	//if f.syn == SyntaxProto3 {
	// TODO: technically you can disable packing?
	return isPackable[f.fd.GetType()]
	//}
	//return f.fd.GetOptions().GetPacked()
}

func (f field) labeledType() string {
	if f.isMap {
		k, v := f.mapFields()
		return fmt.Sprintf("Map<%s, %s>", k.tsType(), v.tsType())
	}
	if f.isRepeated() {
		return f.tsType() + "[]"
	}
	if f.isMessage() {
		return f.tsType() + " | null"
	}
	return f.tsType()
}

func (f field) isMessage() bool {
	return f.fd.GetType() == desc.FieldDescriptorProto_TYPE_MESSAGE || f.fd.GetType() == desc.FieldDescriptorProto_TYPE_GROUP
}

func (f field) isRepeated() bool {
	return *f.fd.Label == desc.FieldDescriptorProto_LABEL_REPEATED
}

// Default is 0
var writeWireType = map[desc.FieldDescriptorProto_Type]int{
	desc.FieldDescriptorProto_TYPE_FLOAT:    5,
	desc.FieldDescriptorProto_TYPE_DOUBLE:   1,
	desc.FieldDescriptorProto_TYPE_FIXED32:  5,
	desc.FieldDescriptorProto_TYPE_SFIXED32: 5,
	desc.FieldDescriptorProto_TYPE_FIXED64:  1,
	desc.FieldDescriptorProto_TYPE_SFIXED64: 1,
	desc.FieldDescriptorProto_TYPE_STRING:   2,
	desc.FieldDescriptorProto_TYPE_BYTES:    2,
	desc.FieldDescriptorProto_TYPE_MESSAGE:  2,
	desc.FieldDescriptorProto_TYPE_GROUP:    2,
}

var isPackable = map[desc.FieldDescriptorProto_Type]bool{
	desc.FieldDescriptorProto_TYPE_INT64:    true,
	desc.FieldDescriptorProto_TYPE_INT32:    true,
	desc.FieldDescriptorProto_TYPE_UINT64:   true,
	desc.FieldDescriptorProto_TYPE_UINT32:   true,
	desc.FieldDescriptorProto_TYPE_SINT64:   true,
	desc.FieldDescriptorProto_TYPE_SINT32:   true,
	desc.FieldDescriptorProto_TYPE_FLOAT:    true,
	desc.FieldDescriptorProto_TYPE_DOUBLE:   true,
	desc.FieldDescriptorProto_TYPE_FIXED32:  true,
	desc.FieldDescriptorProto_TYPE_SFIXED32: true,
	desc.FieldDescriptorProto_TYPE_FIXED64:  true,
	desc.FieldDescriptorProto_TYPE_SFIXED64: true,
	desc.FieldDescriptorProto_TYPE_BOOL:     true,
	desc.FieldDescriptorProto_TYPE_ENUM:     true,
}

func (f field) writeDecoder(w *writer, dec, wt string) {
	if f.isMap {
		w.p("{")
		w.p("let obj = new %s();", f.typeTsName)
		w.p("obj.MergeFrom(%s.readDecoder());", dec)

		_, v := f.mapFields()
		if v.isMessage() {
			w.p("this.%s.set(obj.key, obj.value == null ? new %s() : obj.value);", f.varName(), v.tsType())
		} else {
			w.p("this.%s.set(obj.key, obj.value);", f.varName())
		}
		w.p("}")
		// TODO
		return
	}
	if f.isMessage() {
		if f.isRepeated() {
			w.p("{")
			w.p("let obj = new %s();", f.typeTsName)
			w.p("obj.MergeFrom(%s.readDecoder());", dec)
			w.p("this.%s.push(obj)", f.varName())
			w.p("}")
		} else {
			if f.isOneofMember() {
				oo := f.oneof
				w.p("{")
				w.p("let msg = new %s();", f.typeTsName)
				w.p("msg.MergeFrom(%s.readDecoder());", dec)
				w.p("this.%s = new %s.%s(msg);", oo.odp.GetName(), oo.fqNamespace, f.varName())
				w.p("}")
				return
			}
			w.p("if (this.%s == null) this.%s = new %s();", f.varName(), f.varName(), f.typeTsName)
			w.p("this.%s.MergeFrom(%s.readDecoder());", f.varName(), dec)
		}
		return
	}
	reader := ""
	switch f.fd.GetType() {
	case desc.FieldDescriptorProto_TYPE_STRING:
		reader = fmt.Sprintf("%s.readString()", dec)
	case desc.FieldDescriptorProto_TYPE_BYTES:
		reader = fmt.Sprintf("%s.readBytes()", dec)
	case desc.FieldDescriptorProto_TYPE_INT64:
		reader = fmt.Sprintf("%s.readVarintSigned()", dec)
	case desc.FieldDescriptorProto_TYPE_UINT64:
		reader = fmt.Sprintf("%s.readVarint()", dec)
	case desc.FieldDescriptorProto_TYPE_INT32:
		reader = fmt.Sprintf("%s.readVarInt32()", dec)
	case desc.FieldDescriptorProto_TYPE_UINT32:
		reader = fmt.Sprintf("%s.readVarUint32()", dec)
	case desc.FieldDescriptorProto_TYPE_SINT64:
		reader = fmt.Sprintf("%s.readZigZag64()", dec)
	case desc.FieldDescriptorProto_TYPE_SINT32:
		reader = fmt.Sprintf("%s.readZigZag32()", dec)
	case desc.FieldDescriptorProto_TYPE_FLOAT:
		reader = fmt.Sprintf("%s.readFloat()", dec)
	case desc.FieldDescriptorProto_TYPE_DOUBLE:
		reader = fmt.Sprintf("%s.readDouble()", dec)
	case desc.FieldDescriptorProto_TYPE_FIXED32:
		reader = fmt.Sprintf("%s.readUint32()", dec)
	case desc.FieldDescriptorProto_TYPE_SFIXED32:
		reader = fmt.Sprintf("%s.readInt32()", dec)
	case desc.FieldDescriptorProto_TYPE_FIXED64:
		reader = fmt.Sprintf("%s.readUint64()", dec)
	case desc.FieldDescriptorProto_TYPE_SFIXED64:
		reader = fmt.Sprintf("%s.readInt64()", dec)
	case desc.FieldDescriptorProto_TYPE_BOOL:
		reader = fmt.Sprintf("%s.readBool()", dec)
	case desc.FieldDescriptorProto_TYPE_ENUM:
		reader = fmt.Sprintf("%s.readVarintSignedAsNumber()", dec)
	default:
		panic(fmt.Errorf("unknown reader for fd type: %+v", f.fd.GetType()))
	}
	if f.isOneofMember() {
		oo := f.oneof
		w.p("this.%s = new %s.%s(%s);", oo.odp.GetName(), oo.fqNamespace, f.varName(), reader)
		return
	}
	if !f.isRepeated() {
		w.p("this.%s = %s;", f.varName(), reader)
		return
	}
	packable := isPackable[f.fd.GetType()]
	if packable {
		w.p("if (%s == 2) {", wt)
		w.p("let packed = %s.readDecoder();", dec)
		w.p("while (!packed.isEOF()) {")
		packedReader := strings.Replace(reader, dec, "packed", 1) // heh kinda hacky
		w.p("this.%s.push(%s)", f.varName(), packedReader)
		w.p("}")
		w.p("} else {")
	}
	w.p("this.%s.push(%s)", f.varName(), reader)
	if packable {
		w.p("}")
	}
	// Repeated.
}

func (f field) primitiveWriter(enc string) (string, string) {
	writer := ""
	switch f.fd.GetType() {
	case desc.FieldDescriptorProto_TYPE_STRING:
		writer = fmt.Sprintf("%s.writeString(this.%s)", enc, f.varName())
	case desc.FieldDescriptorProto_TYPE_BYTES:
		writer = fmt.Sprintf("%s.writeBytes(this.%s)", enc, f.varName())
	case desc.FieldDescriptorProto_TYPE_INT64,
		desc.FieldDescriptorProto_TYPE_UINT64:
		writer = fmt.Sprintf("%s.writeVarint(this.%s)", enc, f.varName())
	case desc.FieldDescriptorProto_TYPE_INT32,
		desc.FieldDescriptorProto_TYPE_UINT32:
		writer = fmt.Sprintf("%s.writeNumberAsVarint(this.%s)", enc, f.varName())
	case desc.FieldDescriptorProto_TYPE_SINT64:
		writer = fmt.Sprintf("%s.writeZigZag64(this.%s)", enc, f.varName())
	case desc.FieldDescriptorProto_TYPE_SINT32:
		writer = fmt.Sprintf("%s.writeZigZag32(this.%s)", enc, f.varName())
	case desc.FieldDescriptorProto_TYPE_FLOAT:
		writer = fmt.Sprintf("%s.writeFloat(this.%s)", enc, f.varName())
	case desc.FieldDescriptorProto_TYPE_DOUBLE:
		writer = fmt.Sprintf("%s.writeDouble(this.%s)", enc, f.varName())
	case desc.FieldDescriptorProto_TYPE_FIXED32:
		writer = fmt.Sprintf("%s.writeUint32(this.%s)", enc, f.varName())
	case desc.FieldDescriptorProto_TYPE_SFIXED32:
		writer = fmt.Sprintf("%s.writeInt32(this.%s)", enc, f.varName())
	case desc.FieldDescriptorProto_TYPE_FIXED64:
		writer = fmt.Sprintf("%s.writeUint64(this.%s)", enc, f.varName())
	case desc.FieldDescriptorProto_TYPE_SFIXED64:
		writer = fmt.Sprintf("%s.writeInt64(this.%s)", enc, f.varName())
	case desc.FieldDescriptorProto_TYPE_BOOL:
		writer = fmt.Sprintf("%s.writeBool(this.%s)", enc, f.varName())
	case desc.FieldDescriptorProto_TYPE_ENUM:
		writer = fmt.Sprintf("%s.writeNumberAsVarint(this.%s)", enc, f.varName())
	default:
		panic(fmt.Errorf("unknown primitive writer for fd type: %+v", f.fd.GetType()))
	}
	tagWriter := fmt.Sprintf("%s.writeTag(%d, %d)", enc, f.fd.GetNumber(), writeWireType[f.fd.GetType()])
	return tagWriter, writer
}

func (f field) writeEncoder(w *writer, libMod *modRef, enc string, alwaysEmitDefaultValue bool) {
	if f.isMap {
		w.p("for (const [k, v] of this.%s) {", f.varName())
		w.p("let obj = new %s();", f.typeTsName)
		w.p("obj.key = k;")
		w.p("obj.value = v;")
		w.p("let nested = new %s.Internal.Encoder();", libMod.alias)
		w.p("obj.WriteTo(nested);")
		w.p("%s.writeEncoder(nested, %d);", enc, f.fd.GetNumber())
		w.p("}")
		return
	}

	if f.isMessage() {
		w.p("{")
		if f.isRepeated() {
			w.p("for (const msg of this.%s) {", f.varName())
		} else {
			w.p("const msg = this.%s;", f.varName())
			w.p("if (msg != null) {")
		}
		w.p("let nested = new %s.Internal.Encoder();", libMod.alias)
		w.p("msg.WriteTo(nested);")
		w.p("%s.writeEncoder(nested, %d)", enc, f.fd.GetNumber())
		w.p("}")
		w.p("}")
		return
	}

	tagWriter, writer := f.primitiveWriter(enc)

	if !f.isRepeated() {
		if !alwaysEmitDefaultValue {
			if f.fd.GetType() == desc.FieldDescriptorProto_TYPE_BYTES {
				w.p("if (this.%s.length != 0) {", f.varName())
			} else {
				w.p("if (this.%s != %s) {", f.varName(), f.defaultValue())
			}
		}
		w.p(tagWriter + ";")
		w.p(writer + ";")
		if !alwaysEmitDefaultValue {
			w.p("}")
		}
		return
	}
	// repeated
	// heh kinda hacky.
	repeatWriter := strings.Replace(writer, "this."+f.varName(), "elem", 1)
	if f.isPacked() {
		packedWriter := strings.Replace(repeatWriter, enc, "packed", 1) // heh hax
		w.p("{")
		w.p("const packed = new %s.Internal.Encoder();", libMod.alias)
		w.p("for (let elem of this.%s) {", f.varName())
		w.p(packedWriter + ";")
		w.p("}")
		w.p("%s.writeEncoder(packed, %d);", enc, f.fd.GetNumber())
		w.p("}")
	} else {
		w.p("for (let elem of this.%s) {", f.varName())
		w.p(tagWriter + ";")
		w.p(repeatWriter + ";")
		w.p("}")
	}
}

func writeEnum(w *writer, edp *desc.EnumDescriptorProto, prefixNames []string) {
	// name := strings.Join(append(prefixNames, edp.GetName()), "_")
	if len(prefixNames) > 0 {
		w.p("export namespace %s {", strings.Join(prefixNames, "."))
	}
	w.p("export const enum %s {", edp.GetName())
	for _, v := range edp.Value {
		w.p("%s = %d,", v.GetName(), v.GetNumber())
	}
	w.p("}")
	if len(prefixNames) > 0 {
		w.p("}") // namespace
	}
	w.ln()
}

func writeOneof(w *writer, oo *oneof, libMod *modRef, prefixNames []string) {
	if len(prefixNames) > 0 {
		w.p("export namespace %s {", strings.Join(append(prefixNames, oo.odp.GetName()), "."))
	}

	classNames := []string{fmt.Sprintf("%s.OneofNotSet", libMod.alias)}
	for _, field := range oo.fields {
		w.p("export class %s {", field.varName())
		w.p("static readonly kind = %d;", field.fd.GetNumber())
		w.p("readonly kind = %d;", field.fd.GetNumber())
		w.p("value: %s;", field.labeledType())
		w.p("constructor(v: %s) {", field.labeledType())
		w.p("this.value = v;")
		w.p("}")
		classNames = append(classNames, field.varName())
		w.p("}")
		w.ln()

	}

	union := strings.Join(classNames, " | ")
	w.p("export type %s = %s;", oo.typeName, union)
	w.ln()
	w.p("export function WriteTo(oo: %s, e: %s.Internal.Encoder):void {", oo.typeName, libMod.alias)
	w.p("switch (oo.kind) {")
	for _, f := range oo.fields {
		value := fmt.Sprintf("(oo as %s).value", f.varName())
		w.p("case %d:", f.fd.GetNumber())

		if f.isMessage() {
			w.p("{")
			w.p("let nested = new %s.Internal.Encoder();", libMod.alias)
			w.p("let msg = %s;", value)
			w.p("if (msg != null) {")
			w.p("msg.WriteTo(nested);")
			w.p("}")
			w.p("e.writeEncoder(nested, %d);", f.fd.GetNumber())
			w.p("return")
			w.p("}")
			continue
		}

		tagWriter, writer := f.primitiveWriter("e")
		// heh kinda hacky.
		writer = strings.Replace(writer, "this."+f.varName(), value, 1)

		w.p(tagWriter + ";")
		w.p(writer + ";")
		w.p("return;")
	}

	w.p("}") // switch
	w.p("}") // WriteTo

	if len(prefixNames) > 0 {
		w.p("}") // namespace
	}
	w.ln()
}

func writeDescriptor(w *writer, dp *desc.DescriptorProto, ns *Namespace, mr *moduleResolver, libMod *modRef, prefixNames []string) {
	nextNames := append(prefixNames, dp.GetName())

	// Wrap fields.
	fields := []*field{}
	for _, fd := range dp.Field {
		fields = append(fields, newField(fd, ns, mr))
	}

	// Oneofs: group each field by it's corresponding oneof.
	oneofFields := map[int32][]*field{}
	for _, field := range fields {
		if !field.isOneofMember() {
			continue
		}
		i := field.fd.GetOneofIndex()
		l := oneofFields[i]
		l = append(l, field)
		oneofFields[i] = l
	}

	// Wrap oneofs.
	oneofs := []*oneof{}
	for i, od := range dp.OneofDecl {
		oo := &oneof{
			odp:         od,
			fields:      oneofFields[int32(i)],
			typeName:    "oneof_type",
			fqNamespace: strings.Join(append(nextNames, od.GetName()), "."),
		}
		oneofs = append(oneofs, oo)
	}

	// Now point each field at it's oneof.
	for _, field := range fields {
		if field.isOneofMember() {
			field.oneof = oneofs[field.fd.GetOneofIndex()]
		}
	}

	if len(prefixNames) > 0 {
		w.p("export namespace %s {", strings.Join(prefixNames, "."))
	}

	// Message
	w.p("export class %s implements %s.Message {", dp.GetName(), libMod.alias)
	for _, f := range fields {
		if f.isOneofMember() {
			continue
		}
		w.p("%s: %s;", f.varName(), f.labeledType())
	}
	for _, oo := range oneofs {
		w.p("%s: %s.%s;", oo.odp.GetName(), oo.fqNamespace, oo.typeName)
	}
	w.ln()

	// Constructor
	w.p("constructor() {")
	for _, f := range fields {
		if f.isOneofMember() {
			continue
		}
		w.p("this.%s = %s;", f.varName(), f.defaultValue())
	}
	for _, oo := range oneofs {
		w.p("this.%s = %s.OneofNotSet.singleton;", oo.odp.GetName(), libMod.alias)
	}
	w.p("}") // constructor
	w.ln()

	// MergeFrom
	w.p("MergeFrom(d: %s.Internal.Decoder): void {", libMod.alias)
	w.p("while (!d.isEOF()) {")
	w.p("let [fn, wt] = d.readTag();")
	w.p("switch(fn) {")
	for _, f := range fields {
		w.p("case %d:", f.fd.GetNumber())
		w.pdebug("reading field:%d (%s) wt:${wt}", f.fd.GetNumber(), f.fd.GetName())
		f.writeDecoder(w, "d", "wt")
		w.pdebug("read field:%d (%s)", f.fd.GetNumber(), f.fd.GetName())
		w.p("break;")
	}
	w.p("default:")
	w.pdebug("skipping unknown field:${fn} wt:${wt}")
	w.p("d.skipWireType(wt)")
	w.p("}") // switch
	w.p("}") // while
	w.p("}") // MergeFrom
	w.ln()

	// WriteTo
	if len(fields) < 1 {
		w.p("WriteTo(_: %s.Internal.Encoder): void {}", libMod.alias)
	} else {
		w.p("WriteTo(e: %s.Internal.Encoder): void {", libMod.alias)
		for _, f := range fields {
			if f.isOneofMember() {
				continue
			}
			w.pdebug("maybe writing field %d, (%s)", f.fd.GetNumber(), f.fd.GetName())
			f.writeEncoder(w, libMod, "e", false)
			w.pdebug("maybe wrote field %d, (%s)", f.fd.GetNumber(), f.fd.GetName())
		}
		for _, oo := range oneofs {
			w.p("%s.WriteTo(this.%s, e);", oo.fqNamespace, oo.odp.GetName())
		}

		w.p("}") // WriteTo
	}
	w.p("}") // class

	if len(prefixNames) > 0 {
		w.p("}") // namespace
	}
	w.ln()

	// Write oneofs
	for _, oo := range oneofs {
		writeOneof(w, oo, libMod, nextNames)
	}

	// Write enums.
	for _, edp := range dp.EnumType {
		writeEnum(w, edp, nextNames)
	}

	// Nested types.
	for _, ndp := range dp.NestedType {
		writeDescriptor(w, ndp, ns, mr, libMod, nextNames)
	}
}

type method struct {
	mdp                               *desc.MethodDescriptorProto
	TsName, InputTsName, OutputTsName string
}

func newMethod(mdp *desc.MethodDescriptorProto, ns *Namespace, mr *moduleResolver) method {
	m := method{mdp: mdp}
	m.TsName = mdp.GetName()

	_, typeName, _, typeFdp := ns.FindFullyQualifiedName(mdp.GetInputType())
	m.InputTsName = typeName
	if mod := mr.ToRelativeModule(typeFdp); mod != nil {
		m.InputTsName = mod.alias + "." + m.InputTsName
	}

	_, typeName, _, typeFdp = ns.FindFullyQualifiedName(mdp.GetOutputType())
	m.OutputTsName = typeName
	if mod := mr.ToRelativeModule(typeFdp); mod != nil {
		m.OutputTsName = mod.alias + "." + m.OutputTsName
	}
	return m
}

func (m method) isStreaming() bool {
	return m.mdp.GetClientStreaming() || m.mdp.GetServerStreaming()
}

func writeService(w *writer, sdp *desc.ServiceDescriptorProto, pkg string, ns *Namespace, mr *moduleResolver, libMod *modRef) {
	methods := []method{}
	for _, mdp := range sdp.Method {
		methods = append(methods, newMethod(mdp, ns, mr))
	}
	fqname := sdp.GetName()
	if pkg != "" {
		fqname = pkg + "." + fqname
	}

	// Client
	w.p("export class %sClient {", sdp.GetName())
	w.p("private cc: %s.Grpc.ClientConn;", libMod.alias)
	w.p("constructor(cc: %s.Grpc.ClientConn) {", libMod.alias)
	w.p("this.cc = cc;")
	w.p("}")
	for _, m := range methods {
		if m.isStreaming() {
			continue
		}
		w.ln()
		w.p("async %s(min: %s, ...co: %s.Grpc.CallOption[]): Promise<%s> {", m.TsName, m.InputTsName, libMod.alias, m.OutputTsName)
		w.p("let mout = new %s();", m.OutputTsName)
		w.p("await this.cc.Invoke('/%s/%s', min, mout, ...co);", fqname, m.mdp.GetName())
		w.p("return mout;")
		w.p("}")
	}
	w.p("}")
}

// writer is a little helper for output printing. It indents code
// appropriately among other things.
type writer struct {
	w io.Writer
	i int
}

func (w *writer) p(format string, a ...interface{}) {
	if strings.HasPrefix(format, "}") {
		w.i--
	}
	i := w.i
	if i < 0 {
		i = 0
	}
	indent := strings.Repeat("  ", i)
	fmt.Fprintf(w.w, indent+format, a...)
	w.ln()
	if strings.HasSuffix(format, "{") {
		w.i++
	}
}

func (w *writer) ln() {
	fmt.Fprintln(w.w)
}

func (w *writer) pdebug(f string, i ...interface{}) {
	if !genDebug {
		return
	}
	w.p("console.log(`[PROTOC-DEBUG] %s`);", fmt.Sprintf(f, i...))
}
