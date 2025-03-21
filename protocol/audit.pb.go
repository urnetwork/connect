// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.35.1
// 	protoc        v4.23.4
// source: audit.proto

package protocol

import (
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	reflect "reflect"
	sync "sync"
)

const (
	// Verify that this generated code is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that runtime/protoimpl is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

type EgressProtocol int32

const (
	EgressProtocol_IP_TCP EgressProtocol = 0
	EgressProtocol_IP_UDP EgressProtocol = 1
)

// Enum value maps for EgressProtocol.
var (
	EgressProtocol_name = map[int32]string{
		0: "IP_TCP",
		1: "IP_UDP",
	}
	EgressProtocol_value = map[string]int32{
		"IP_TCP": 0,
		"IP_UDP": 1,
	}
)

func (x EgressProtocol) Enum() *EgressProtocol {
	p := new(EgressProtocol)
	*p = x
	return p
}

func (x EgressProtocol) String() string {
	return protoimpl.X.EnumStringOf(x.Descriptor(), protoreflect.EnumNumber(x))
}

func (EgressProtocol) Descriptor() protoreflect.EnumDescriptor {
	return file_audit_proto_enumTypes[0].Descriptor()
}

func (EgressProtocol) Type() protoreflect.EnumType {
	return &file_audit_proto_enumTypes[0]
}

func (x EgressProtocol) Number() protoreflect.EnumNumber {
	return protoreflect.EnumNumber(x)
}

// Deprecated: Use EgressProtocol.Descriptor instead.
func (EgressProtocol) EnumDescriptor() ([]byte, []int) {
	return file_audit_proto_rawDescGZIP(), []int{0}
}

// this is sent to the platform
// note the platform does not have prior knowledge and would have to brute force decrypt the audit record
// the provider creates and encrypts the final `AccountRecord` from the partial and discards the partial
type ProviderAudit struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Egress *EncryptedEgressRecord `protobuf:"bytes,1,opt,name=Egress,proto3" json:"Egress,omitempty"`
	// this data is used to encode the associated `AccountRecord`
	// this data is not stored and should be thrown out by the platform
	AccountPartial *AccountRecordPartial `protobuf:"bytes,2,opt,name=AccountPartial,proto3" json:"AccountPartial,omitempty"`
}

func (x *ProviderAudit) Reset() {
	*x = ProviderAudit{}
	mi := &file_audit_proto_msgTypes[0]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *ProviderAudit) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ProviderAudit) ProtoMessage() {}

func (x *ProviderAudit) ProtoReflect() protoreflect.Message {
	mi := &file_audit_proto_msgTypes[0]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ProviderAudit.ProtoReflect.Descriptor instead.
func (*ProviderAudit) Descriptor() ([]byte, []int) {
	return file_audit_proto_rawDescGZIP(), []int{0}
}

func (x *ProviderAudit) GetEgress() *EncryptedEgressRecord {
	if x != nil {
		return x.Egress
	}
	return nil
}

func (x *ProviderAudit) GetAccountPartial() *AccountRecordPartial {
	if x != nil {
		return x.AccountPartial
	}
	return nil
}

// kept local on provider
// time block is UTC in 50ms blocks
// a typical abuse report will have to account for +-60s, or look up about 1200 blocks per record
type EgressKey struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	TimeBlock       uint64         `protobuf:"varint,1,opt,name=TimeBlock,proto3" json:"TimeBlock,omitempty"`
	Protocol        EgressProtocol `protobuf:"varint,2,opt,name=Protocol,proto3,enum=bringyour.EgressProtocol" json:"Protocol,omitempty"`
	SourceIp        []byte         `protobuf:"bytes,3,opt,name=SourceIp,proto3" json:"SourceIp,omitempty"`
	SourcePort      uint32         `protobuf:"varint,4,opt,name=SourcePort,proto3" json:"SourcePort,omitempty"`
	DestinationIp   []byte         `protobuf:"bytes,5,opt,name=DestinationIp,proto3" json:"DestinationIp,omitempty"`
	DestinationPort uint32         `protobuf:"varint,6,opt,name=DestinationPort,proto3" json:"DestinationPort,omitempty"`
}

func (x *EgressKey) Reset() {
	*x = EgressKey{}
	mi := &file_audit_proto_msgTypes[1]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *EgressKey) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*EgressKey) ProtoMessage() {}

func (x *EgressKey) ProtoReflect() protoreflect.Message {
	mi := &file_audit_proto_msgTypes[1]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use EgressKey.ProtoReflect.Descriptor instead.
func (*EgressKey) Descriptor() ([]byte, []int) {
	return file_audit_proto_rawDescGZIP(), []int{1}
}

func (x *EgressKey) GetTimeBlock() uint64 {
	if x != nil {
		return x.TimeBlock
	}
	return 0
}

func (x *EgressKey) GetProtocol() EgressProtocol {
	if x != nil {
		return x.Protocol
	}
	return EgressProtocol_IP_TCP
}

func (x *EgressKey) GetSourceIp() []byte {
	if x != nil {
		return x.SourceIp
	}
	return nil
}

func (x *EgressKey) GetSourcePort() uint32 {
	if x != nil {
		return x.SourcePort
	}
	return 0
}

func (x *EgressKey) GetDestinationIp() []byte {
	if x != nil {
		return x.DestinationIp
	}
	return nil
}

func (x *EgressKey) GetDestinationPort() uint32 {
	if x != nil {
		return x.DestinationPort
	}
	return 0
}

// kept local on provider
type EgressKeyWithSalt struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Key *EgressKey `protobuf:"bytes,1,opt,name=Key,proto3" json:"Key,omitempty"`
	// 128-bit
	Salt []byte `protobuf:"bytes,2,opt,name=Salt,proto3" json:"Salt,omitempty"`
}

func (x *EgressKeyWithSalt) Reset() {
	*x = EgressKeyWithSalt{}
	mi := &file_audit_proto_msgTypes[2]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *EgressKeyWithSalt) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*EgressKeyWithSalt) ProtoMessage() {}

func (x *EgressKeyWithSalt) ProtoReflect() protoreflect.Message {
	mi := &file_audit_proto_msgTypes[2]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use EgressKeyWithSalt.ProtoReflect.Descriptor instead.
func (*EgressKeyWithSalt) Descriptor() ([]byte, []int) {
	return file_audit_proto_rawDescGZIP(), []int{2}
}

func (x *EgressKeyWithSalt) GetKey() *EgressKey {
	if x != nil {
		return x.Key
	}
	return nil
}

func (x *EgressKeyWithSalt) GetSalt() []byte {
	if x != nil {
		return x.Salt
	}
	return nil
}

// this created on the provider so that the platform cannot see the raw data
// a complete audit record is an `EgressRecord` and an `AccountRecord`
type EncryptedEgressRecord struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	// 128-bit
	Salt []byte `protobuf:"bytes,1,opt,name=Salt,proto3" json:"Salt,omitempty"`
	// Argon2id of `EgressKeyWithSalt`
	KeyWithSaltHash []byte `protobuf:"bytes,2,opt,name=KeyWithSaltHash,proto3" json:"KeyWithSaltHash,omitempty"`
	// AES256 using `EgressRecordSecret`
	EncryptedEgress []byte `protobuf:"bytes,3,opt,name=EncryptedEgress,proto3" json:"EncryptedEgress,omitempty"`
}

func (x *EncryptedEgressRecord) Reset() {
	*x = EncryptedEgressRecord{}
	mi := &file_audit_proto_msgTypes[3]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *EncryptedEgressRecord) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*EncryptedEgressRecord) ProtoMessage() {}

func (x *EncryptedEgressRecord) ProtoReflect() protoreflect.Message {
	mi := &file_audit_proto_msgTypes[3]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use EncryptedEgressRecord.ProtoReflect.Descriptor instead.
func (*EncryptedEgressRecord) Descriptor() ([]byte, []int) {
	return file_audit_proto_rawDescGZIP(), []int{3}
}

func (x *EncryptedEgressRecord) GetSalt() []byte {
	if x != nil {
		return x.Salt
	}
	return nil
}

func (x *EncryptedEgressRecord) GetKeyWithSaltHash() []byte {
	if x != nil {
		return x.KeyWithSaltHash
	}
	return nil
}

func (x *EncryptedEgressRecord) GetEncryptedEgress() []byte {
	if x != nil {
		return x.EncryptedEgress
	}
	return nil
}

type AccountRecordPartial struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	// ulid
	ContractId          []byte `protobuf:"bytes,4,opt,name=ContractId,proto3" json:"ContractId,omitempty"`
	AccountRecordSecret []byte `protobuf:"bytes,5,opt,name=AccountRecordSecret,proto3" json:"AccountRecordSecret,omitempty"`
}

func (x *AccountRecordPartial) Reset() {
	*x = AccountRecordPartial{}
	mi := &file_audit_proto_msgTypes[4]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *AccountRecordPartial) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*AccountRecordPartial) ProtoMessage() {}

func (x *AccountRecordPartial) ProtoReflect() protoreflect.Message {
	mi := &file_audit_proto_msgTypes[4]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use AccountRecordPartial.ProtoReflect.Descriptor instead.
func (*AccountRecordPartial) Descriptor() ([]byte, []int) {
	return file_audit_proto_rawDescGZIP(), []int{4}
}

func (x *AccountRecordPartial) GetContractId() []byte {
	if x != nil {
		return x.ContractId
	}
	return nil
}

func (x *AccountRecordPartial) GetAccountRecordSecret() []byte {
	if x != nil {
		return x.AccountRecordSecret
	}
	return nil
}

type EgressRecordSecret struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Key *EgressKey `protobuf:"bytes,1,opt,name=Key,proto3" json:"Key,omitempty"`
	// 128-bit
	Salt []byte `protobuf:"bytes,2,opt,name=Salt,proto3" json:"Salt,omitempty"`
	// 24-bit
	Nonce []byte `protobuf:"bytes,3,opt,name=Nonce,proto3" json:"Nonce,omitempty"`
}

func (x *EgressRecordSecret) Reset() {
	*x = EgressRecordSecret{}
	mi := &file_audit_proto_msgTypes[5]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *EgressRecordSecret) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*EgressRecordSecret) ProtoMessage() {}

func (x *EgressRecordSecret) ProtoReflect() protoreflect.Message {
	mi := &file_audit_proto_msgTypes[5]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use EgressRecordSecret.ProtoReflect.Descriptor instead.
func (*EgressRecordSecret) Descriptor() ([]byte, []int) {
	return file_audit_proto_rawDescGZIP(), []int{5}
}

func (x *EgressRecordSecret) GetKey() *EgressKey {
	if x != nil {
		return x.Key
	}
	return nil
}

func (x *EgressRecordSecret) GetSalt() []byte {
	if x != nil {
		return x.Salt
	}
	return nil
}

func (x *EgressRecordSecret) GetNonce() []byte {
	if x != nil {
		return x.Nonce
	}
	return nil
}

type EgressRecord struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	Key                 *EgressKey `protobuf:"bytes,1,opt,name=Key,proto3" json:"Key,omitempty"`
	StartTime           uint64     `protobuf:"varint,2,opt,name=StartTime,proto3" json:"StartTime,omitempty"`
	EndTime             *uint64    `protobuf:"varint,3,opt,name=EndTime,proto3,oneof" json:"EndTime,omitempty"`
	AccountRecordSecret []byte     `protobuf:"bytes,4,opt,name=AccountRecordSecret,proto3" json:"AccountRecordSecret,omitempty"`
}

func (x *EgressRecord) Reset() {
	*x = EgressRecord{}
	mi := &file_audit_proto_msgTypes[6]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *EgressRecord) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*EgressRecord) ProtoMessage() {}

func (x *EgressRecord) ProtoReflect() protoreflect.Message {
	mi := &file_audit_proto_msgTypes[6]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use EgressRecord.ProtoReflect.Descriptor instead.
func (*EgressRecord) Descriptor() ([]byte, []int) {
	return file_audit_proto_rawDescGZIP(), []int{6}
}

func (x *EgressRecord) GetKey() *EgressKey {
	if x != nil {
		return x.Key
	}
	return nil
}

func (x *EgressRecord) GetStartTime() uint64 {
	if x != nil {
		return x.StartTime
	}
	return 0
}

func (x *EgressRecord) GetEndTime() uint64 {
	if x != nil && x.EndTime != nil {
		return *x.EndTime
	}
	return 0
}

func (x *EgressRecord) GetAccountRecordSecret() []byte {
	if x != nil {
		return x.AccountRecordSecret
	}
	return nil
}

type AccountRecord struct {
	state         protoimpl.MessageState
	sizeCache     protoimpl.SizeCache
	unknownFields protoimpl.UnknownFields

	// ulid
	ContractId []byte `protobuf:"bytes,1,opt,name=ContractId,proto3" json:"ContractId,omitempty"`
	// ulid
	ClientAccountId []byte `protobuf:"bytes,2,opt,name=ClientAccountId,proto3" json:"ClientAccountId,omitempty"`
	// ulid
	ProviderAccountId []byte `protobuf:"bytes,3,opt,name=ProviderAccountId,proto3" json:"ProviderAccountId,omitempty"`
}

func (x *AccountRecord) Reset() {
	*x = AccountRecord{}
	mi := &file_audit_proto_msgTypes[7]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *AccountRecord) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*AccountRecord) ProtoMessage() {}

func (x *AccountRecord) ProtoReflect() protoreflect.Message {
	mi := &file_audit_proto_msgTypes[7]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use AccountRecord.ProtoReflect.Descriptor instead.
func (*AccountRecord) Descriptor() ([]byte, []int) {
	return file_audit_proto_rawDescGZIP(), []int{7}
}

func (x *AccountRecord) GetContractId() []byte {
	if x != nil {
		return x.ContractId
	}
	return nil
}

func (x *AccountRecord) GetClientAccountId() []byte {
	if x != nil {
		return x.ClientAccountId
	}
	return nil
}

func (x *AccountRecord) GetProviderAccountId() []byte {
	if x != nil {
		return x.ProviderAccountId
	}
	return nil
}

var File_audit_proto protoreflect.FileDescriptor

var file_audit_proto_rawDesc = []byte{
	0x0a, 0x0b, 0x61, 0x75, 0x64, 0x69, 0x74, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x12, 0x09, 0x62,
	0x72, 0x69, 0x6e, 0x67, 0x79, 0x6f, 0x75, 0x72, 0x22, 0x92, 0x01, 0x0a, 0x0d, 0x50, 0x72, 0x6f,
	0x76, 0x69, 0x64, 0x65, 0x72, 0x41, 0x75, 0x64, 0x69, 0x74, 0x12, 0x38, 0x0a, 0x06, 0x45, 0x67,
	0x72, 0x65, 0x73, 0x73, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x20, 0x2e, 0x62, 0x72, 0x69,
	0x6e, 0x67, 0x79, 0x6f, 0x75, 0x72, 0x2e, 0x45, 0x6e, 0x63, 0x72, 0x79, 0x70, 0x74, 0x65, 0x64,
	0x45, 0x67, 0x72, 0x65, 0x73, 0x73, 0x52, 0x65, 0x63, 0x6f, 0x72, 0x64, 0x52, 0x06, 0x45, 0x67,
	0x72, 0x65, 0x73, 0x73, 0x12, 0x47, 0x0a, 0x0e, 0x41, 0x63, 0x63, 0x6f, 0x75, 0x6e, 0x74, 0x50,
	0x61, 0x72, 0x74, 0x69, 0x61, 0x6c, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x1f, 0x2e, 0x62,
	0x72, 0x69, 0x6e, 0x67, 0x79, 0x6f, 0x75, 0x72, 0x2e, 0x41, 0x63, 0x63, 0x6f, 0x75, 0x6e, 0x74,
	0x52, 0x65, 0x63, 0x6f, 0x72, 0x64, 0x50, 0x61, 0x72, 0x74, 0x69, 0x61, 0x6c, 0x52, 0x0e, 0x41,
	0x63, 0x63, 0x6f, 0x75, 0x6e, 0x74, 0x50, 0x61, 0x72, 0x74, 0x69, 0x61, 0x6c, 0x22, 0xec, 0x01,
	0x0a, 0x09, 0x45, 0x67, 0x72, 0x65, 0x73, 0x73, 0x4b, 0x65, 0x79, 0x12, 0x1c, 0x0a, 0x09, 0x54,
	0x69, 0x6d, 0x65, 0x42, 0x6c, 0x6f, 0x63, 0x6b, 0x18, 0x01, 0x20, 0x01, 0x28, 0x04, 0x52, 0x09,
	0x54, 0x69, 0x6d, 0x65, 0x42, 0x6c, 0x6f, 0x63, 0x6b, 0x12, 0x35, 0x0a, 0x08, 0x50, 0x72, 0x6f,
	0x74, 0x6f, 0x63, 0x6f, 0x6c, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0e, 0x32, 0x19, 0x2e, 0x62, 0x72,
	0x69, 0x6e, 0x67, 0x79, 0x6f, 0x75, 0x72, 0x2e, 0x45, 0x67, 0x72, 0x65, 0x73, 0x73, 0x50, 0x72,
	0x6f, 0x74, 0x6f, 0x63, 0x6f, 0x6c, 0x52, 0x08, 0x50, 0x72, 0x6f, 0x74, 0x6f, 0x63, 0x6f, 0x6c,
	0x12, 0x1a, 0x0a, 0x08, 0x53, 0x6f, 0x75, 0x72, 0x63, 0x65, 0x49, 0x70, 0x18, 0x03, 0x20, 0x01,
	0x28, 0x0c, 0x52, 0x08, 0x53, 0x6f, 0x75, 0x72, 0x63, 0x65, 0x49, 0x70, 0x12, 0x1e, 0x0a, 0x0a,
	0x53, 0x6f, 0x75, 0x72, 0x63, 0x65, 0x50, 0x6f, 0x72, 0x74, 0x18, 0x04, 0x20, 0x01, 0x28, 0x0d,
	0x52, 0x0a, 0x53, 0x6f, 0x75, 0x72, 0x63, 0x65, 0x50, 0x6f, 0x72, 0x74, 0x12, 0x24, 0x0a, 0x0d,
	0x44, 0x65, 0x73, 0x74, 0x69, 0x6e, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x49, 0x70, 0x18, 0x05, 0x20,
	0x01, 0x28, 0x0c, 0x52, 0x0d, 0x44, 0x65, 0x73, 0x74, 0x69, 0x6e, 0x61, 0x74, 0x69, 0x6f, 0x6e,
	0x49, 0x70, 0x12, 0x28, 0x0a, 0x0f, 0x44, 0x65, 0x73, 0x74, 0x69, 0x6e, 0x61, 0x74, 0x69, 0x6f,
	0x6e, 0x50, 0x6f, 0x72, 0x74, 0x18, 0x06, 0x20, 0x01, 0x28, 0x0d, 0x52, 0x0f, 0x44, 0x65, 0x73,
	0x74, 0x69, 0x6e, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x50, 0x6f, 0x72, 0x74, 0x22, 0x4f, 0x0a, 0x11,
	0x45, 0x67, 0x72, 0x65, 0x73, 0x73, 0x4b, 0x65, 0x79, 0x57, 0x69, 0x74, 0x68, 0x53, 0x61, 0x6c,
	0x74, 0x12, 0x26, 0x0a, 0x03, 0x4b, 0x65, 0x79, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x14,
	0x2e, 0x62, 0x72, 0x69, 0x6e, 0x67, 0x79, 0x6f, 0x75, 0x72, 0x2e, 0x45, 0x67, 0x72, 0x65, 0x73,
	0x73, 0x4b, 0x65, 0x79, 0x52, 0x03, 0x4b, 0x65, 0x79, 0x12, 0x12, 0x0a, 0x04, 0x53, 0x61, 0x6c,
	0x74, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x04, 0x53, 0x61, 0x6c, 0x74, 0x22, 0x7f, 0x0a,
	0x15, 0x45, 0x6e, 0x63, 0x72, 0x79, 0x70, 0x74, 0x65, 0x64, 0x45, 0x67, 0x72, 0x65, 0x73, 0x73,
	0x52, 0x65, 0x63, 0x6f, 0x72, 0x64, 0x12, 0x12, 0x0a, 0x04, 0x53, 0x61, 0x6c, 0x74, 0x18, 0x01,
	0x20, 0x01, 0x28, 0x0c, 0x52, 0x04, 0x53, 0x61, 0x6c, 0x74, 0x12, 0x28, 0x0a, 0x0f, 0x4b, 0x65,
	0x79, 0x57, 0x69, 0x74, 0x68, 0x53, 0x61, 0x6c, 0x74, 0x48, 0x61, 0x73, 0x68, 0x18, 0x02, 0x20,
	0x01, 0x28, 0x0c, 0x52, 0x0f, 0x4b, 0x65, 0x79, 0x57, 0x69, 0x74, 0x68, 0x53, 0x61, 0x6c, 0x74,
	0x48, 0x61, 0x73, 0x68, 0x12, 0x28, 0x0a, 0x0f, 0x45, 0x6e, 0x63, 0x72, 0x79, 0x70, 0x74, 0x65,
	0x64, 0x45, 0x67, 0x72, 0x65, 0x73, 0x73, 0x18, 0x03, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x0f, 0x45,
	0x6e, 0x63, 0x72, 0x79, 0x70, 0x74, 0x65, 0x64, 0x45, 0x67, 0x72, 0x65, 0x73, 0x73, 0x22, 0x68,
	0x0a, 0x14, 0x41, 0x63, 0x63, 0x6f, 0x75, 0x6e, 0x74, 0x52, 0x65, 0x63, 0x6f, 0x72, 0x64, 0x50,
	0x61, 0x72, 0x74, 0x69, 0x61, 0x6c, 0x12, 0x1e, 0x0a, 0x0a, 0x43, 0x6f, 0x6e, 0x74, 0x72, 0x61,
	0x63, 0x74, 0x49, 0x64, 0x18, 0x04, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x0a, 0x43, 0x6f, 0x6e, 0x74,
	0x72, 0x61, 0x63, 0x74, 0x49, 0x64, 0x12, 0x30, 0x0a, 0x13, 0x41, 0x63, 0x63, 0x6f, 0x75, 0x6e,
	0x74, 0x52, 0x65, 0x63, 0x6f, 0x72, 0x64, 0x53, 0x65, 0x63, 0x72, 0x65, 0x74, 0x18, 0x05, 0x20,
	0x01, 0x28, 0x0c, 0x52, 0x13, 0x41, 0x63, 0x63, 0x6f, 0x75, 0x6e, 0x74, 0x52, 0x65, 0x63, 0x6f,
	0x72, 0x64, 0x53, 0x65, 0x63, 0x72, 0x65, 0x74, 0x22, 0x66, 0x0a, 0x12, 0x45, 0x67, 0x72, 0x65,
	0x73, 0x73, 0x52, 0x65, 0x63, 0x6f, 0x72, 0x64, 0x53, 0x65, 0x63, 0x72, 0x65, 0x74, 0x12, 0x26,
	0x0a, 0x03, 0x4b, 0x65, 0x79, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x14, 0x2e, 0x62, 0x72,
	0x69, 0x6e, 0x67, 0x79, 0x6f, 0x75, 0x72, 0x2e, 0x45, 0x67, 0x72, 0x65, 0x73, 0x73, 0x4b, 0x65,
	0x79, 0x52, 0x03, 0x4b, 0x65, 0x79, 0x12, 0x12, 0x0a, 0x04, 0x53, 0x61, 0x6c, 0x74, 0x18, 0x02,
	0x20, 0x01, 0x28, 0x0c, 0x52, 0x04, 0x53, 0x61, 0x6c, 0x74, 0x12, 0x14, 0x0a, 0x05, 0x4e, 0x6f,
	0x6e, 0x63, 0x65, 0x18, 0x03, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x05, 0x4e, 0x6f, 0x6e, 0x63, 0x65,
	0x22, 0xb1, 0x01, 0x0a, 0x0c, 0x45, 0x67, 0x72, 0x65, 0x73, 0x73, 0x52, 0x65, 0x63, 0x6f, 0x72,
	0x64, 0x12, 0x26, 0x0a, 0x03, 0x4b, 0x65, 0x79, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x14,
	0x2e, 0x62, 0x72, 0x69, 0x6e, 0x67, 0x79, 0x6f, 0x75, 0x72, 0x2e, 0x45, 0x67, 0x72, 0x65, 0x73,
	0x73, 0x4b, 0x65, 0x79, 0x52, 0x03, 0x4b, 0x65, 0x79, 0x12, 0x1c, 0x0a, 0x09, 0x53, 0x74, 0x61,
	0x72, 0x74, 0x54, 0x69, 0x6d, 0x65, 0x18, 0x02, 0x20, 0x01, 0x28, 0x04, 0x52, 0x09, 0x53, 0x74,
	0x61, 0x72, 0x74, 0x54, 0x69, 0x6d, 0x65, 0x12, 0x1d, 0x0a, 0x07, 0x45, 0x6e, 0x64, 0x54, 0x69,
	0x6d, 0x65, 0x18, 0x03, 0x20, 0x01, 0x28, 0x04, 0x48, 0x00, 0x52, 0x07, 0x45, 0x6e, 0x64, 0x54,
	0x69, 0x6d, 0x65, 0x88, 0x01, 0x01, 0x12, 0x30, 0x0a, 0x13, 0x41, 0x63, 0x63, 0x6f, 0x75, 0x6e,
	0x74, 0x52, 0x65, 0x63, 0x6f, 0x72, 0x64, 0x53, 0x65, 0x63, 0x72, 0x65, 0x74, 0x18, 0x04, 0x20,
	0x01, 0x28, 0x0c, 0x52, 0x13, 0x41, 0x63, 0x63, 0x6f, 0x75, 0x6e, 0x74, 0x52, 0x65, 0x63, 0x6f,
	0x72, 0x64, 0x53, 0x65, 0x63, 0x72, 0x65, 0x74, 0x42, 0x0a, 0x0a, 0x08, 0x5f, 0x45, 0x6e, 0x64,
	0x54, 0x69, 0x6d, 0x65, 0x22, 0x87, 0x01, 0x0a, 0x0d, 0x41, 0x63, 0x63, 0x6f, 0x75, 0x6e, 0x74,
	0x52, 0x65, 0x63, 0x6f, 0x72, 0x64, 0x12, 0x1e, 0x0a, 0x0a, 0x43, 0x6f, 0x6e, 0x74, 0x72, 0x61,
	0x63, 0x74, 0x49, 0x64, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x0a, 0x43, 0x6f, 0x6e, 0x74,
	0x72, 0x61, 0x63, 0x74, 0x49, 0x64, 0x12, 0x28, 0x0a, 0x0f, 0x43, 0x6c, 0x69, 0x65, 0x6e, 0x74,
	0x41, 0x63, 0x63, 0x6f, 0x75, 0x6e, 0x74, 0x49, 0x64, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0c, 0x52,
	0x0f, 0x43, 0x6c, 0x69, 0x65, 0x6e, 0x74, 0x41, 0x63, 0x63, 0x6f, 0x75, 0x6e, 0x74, 0x49, 0x64,
	0x12, 0x2c, 0x0a, 0x11, 0x50, 0x72, 0x6f, 0x76, 0x69, 0x64, 0x65, 0x72, 0x41, 0x63, 0x63, 0x6f,
	0x75, 0x6e, 0x74, 0x49, 0x64, 0x18, 0x03, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x11, 0x50, 0x72, 0x6f,
	0x76, 0x69, 0x64, 0x65, 0x72, 0x41, 0x63, 0x63, 0x6f, 0x75, 0x6e, 0x74, 0x49, 0x64, 0x2a, 0x28,
	0x0a, 0x0e, 0x45, 0x67, 0x72, 0x65, 0x73, 0x73, 0x50, 0x72, 0x6f, 0x74, 0x6f, 0x63, 0x6f, 0x6c,
	0x12, 0x0a, 0x0a, 0x06, 0x49, 0x50, 0x5f, 0x54, 0x43, 0x50, 0x10, 0x00, 0x12, 0x0a, 0x0a, 0x06,
	0x49, 0x50, 0x5f, 0x55, 0x44, 0x50, 0x10, 0x01, 0x42, 0x27, 0x5a, 0x25, 0x67, 0x69, 0x74, 0x68,
	0x75, 0x62, 0x2e, 0x63, 0x6f, 0x6d, 0x2f, 0x75, 0x72, 0x6e, 0x65, 0x74, 0x77, 0x6f, 0x72, 0x6b,
	0x2f, 0x63, 0x6f, 0x6e, 0x6e, 0x65, 0x63, 0x74, 0x2f, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x63, 0x6f,
	0x6c, 0x62, 0x06, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x33,
}

var (
	file_audit_proto_rawDescOnce sync.Once
	file_audit_proto_rawDescData = file_audit_proto_rawDesc
)

func file_audit_proto_rawDescGZIP() []byte {
	file_audit_proto_rawDescOnce.Do(func() {
		file_audit_proto_rawDescData = protoimpl.X.CompressGZIP(file_audit_proto_rawDescData)
	})
	return file_audit_proto_rawDescData
}

var file_audit_proto_enumTypes = make([]protoimpl.EnumInfo, 1)
var file_audit_proto_msgTypes = make([]protoimpl.MessageInfo, 8)
var file_audit_proto_goTypes = []any{
	(EgressProtocol)(0),           // 0: bringyour.EgressProtocol
	(*ProviderAudit)(nil),         // 1: bringyour.ProviderAudit
	(*EgressKey)(nil),             // 2: bringyour.EgressKey
	(*EgressKeyWithSalt)(nil),     // 3: bringyour.EgressKeyWithSalt
	(*EncryptedEgressRecord)(nil), // 4: bringyour.EncryptedEgressRecord
	(*AccountRecordPartial)(nil),  // 5: bringyour.AccountRecordPartial
	(*EgressRecordSecret)(nil),    // 6: bringyour.EgressRecordSecret
	(*EgressRecord)(nil),          // 7: bringyour.EgressRecord
	(*AccountRecord)(nil),         // 8: bringyour.AccountRecord
}
var file_audit_proto_depIdxs = []int32{
	4, // 0: bringyour.ProviderAudit.Egress:type_name -> bringyour.EncryptedEgressRecord
	5, // 1: bringyour.ProviderAudit.AccountPartial:type_name -> bringyour.AccountRecordPartial
	0, // 2: bringyour.EgressKey.Protocol:type_name -> bringyour.EgressProtocol
	2, // 3: bringyour.EgressKeyWithSalt.Key:type_name -> bringyour.EgressKey
	2, // 4: bringyour.EgressRecordSecret.Key:type_name -> bringyour.EgressKey
	2, // 5: bringyour.EgressRecord.Key:type_name -> bringyour.EgressKey
	6, // [6:6] is the sub-list for method output_type
	6, // [6:6] is the sub-list for method input_type
	6, // [6:6] is the sub-list for extension type_name
	6, // [6:6] is the sub-list for extension extendee
	0, // [0:6] is the sub-list for field type_name
}

func init() { file_audit_proto_init() }
func file_audit_proto_init() {
	if File_audit_proto != nil {
		return
	}
	file_audit_proto_msgTypes[6].OneofWrappers = []any{}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: file_audit_proto_rawDesc,
			NumEnums:      1,
			NumMessages:   8,
			NumExtensions: 0,
			NumServices:   0,
		},
		GoTypes:           file_audit_proto_goTypes,
		DependencyIndexes: file_audit_proto_depIdxs,
		EnumInfos:         file_audit_proto_enumTypes,
		MessageInfos:      file_audit_proto_msgTypes,
	}.Build()
	File_audit_proto = out.File
	file_audit_proto_rawDesc = nil
	file_audit_proto_goTypes = nil
	file_audit_proto_depIdxs = nil
}
