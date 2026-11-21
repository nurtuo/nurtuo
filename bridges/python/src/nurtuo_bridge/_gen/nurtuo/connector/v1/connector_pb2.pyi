from google.protobuf import struct_pb2 as _struct_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class Host(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    HOST_UNSPECIFIED: _ClassVar[Host]
    HOST_GO_NATIVE: _ClassVar[Host]
    HOST_BRIDGE_JVM: _ClassVar[Host]
    HOST_BRIDGE_PYTHON: _ClassVar[Host]
    HOST_BRIDGE_DOTNET: _ClassVar[Host]
    HOST_WASM: _ClassVar[Host]

class OperationType(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    OPERATION_TYPE_UNSPECIFIED: _ClassVar[OperationType]
    OPERATION_TYPE_GET: _ClassVar[OperationType]
    OPERATION_TYPE_QUERY: _ClassVar[OperationType]
    OPERATION_TYPE_CREATE: _ClassVar[OperationType]
    OPERATION_TYPE_UPDATE: _ClassVar[OperationType]
    OPERATION_TYPE_UPSERT: _ClassVar[OperationType]
    OPERATION_TYPE_DELETE: _ClassVar[OperationType]
    OPERATION_TYPE_EXECUTE: _ClassVar[OperationType]
    OPERATION_TYPE_LISTEN: _ClassVar[OperationType]

class DocumentStatus(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    DOCUMENT_STATUS_UNSPECIFIED: _ClassVar[DocumentStatus]
    DOCUMENT_STATUS_SUCCESS: _ClassVar[DocumentStatus]
    DOCUMENT_STATUS_APPLICATION_ERROR: _ClassVar[DocumentStatus]
    DOCUMENT_STATUS_FAILURE: _ClassVar[DocumentStatus]
HOST_UNSPECIFIED: Host
HOST_GO_NATIVE: Host
HOST_BRIDGE_JVM: Host
HOST_BRIDGE_PYTHON: Host
HOST_BRIDGE_DOTNET: Host
HOST_WASM: Host
OPERATION_TYPE_UNSPECIFIED: OperationType
OPERATION_TYPE_GET: OperationType
OPERATION_TYPE_QUERY: OperationType
OPERATION_TYPE_CREATE: OperationType
OPERATION_TYPE_UPDATE: OperationType
OPERATION_TYPE_UPSERT: OperationType
OPERATION_TYPE_DELETE: OperationType
OPERATION_TYPE_EXECUTE: OperationType
OPERATION_TYPE_LISTEN: OperationType
DOCUMENT_STATUS_UNSPECIFIED: DocumentStatus
DOCUMENT_STATUS_SUCCESS: DocumentStatus
DOCUMENT_STATUS_APPLICATION_ERROR: DocumentStatus
DOCUMENT_STATUS_FAILURE: DocumentStatus

class DescribeRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class DescribeResponse(_message.Message):
    __slots__ = ("descriptor",)
    DESCRIPTOR_FIELD_NUMBER: _ClassVar[int]
    descriptor: Descriptor
    def __init__(self, descriptor: _Optional[_Union[Descriptor, _Mapping]] = ...) -> None: ...

class Descriptor(_message.Message):
    __slots__ = ("id", "version", "host", "display_name", "connection_schema", "operations", "tracked_properties", "capabilities")
    ID_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    HOST_FIELD_NUMBER: _ClassVar[int]
    DISPLAY_NAME_FIELD_NUMBER: _ClassVar[int]
    CONNECTION_SCHEMA_FIELD_NUMBER: _ClassVar[int]
    OPERATIONS_FIELD_NUMBER: _ClassVar[int]
    TRACKED_PROPERTIES_FIELD_NUMBER: _ClassVar[int]
    CAPABILITIES_FIELD_NUMBER: _ClassVar[int]
    id: str
    version: str
    host: Host
    display_name: str
    connection_schema: _struct_pb2.Struct
    operations: _containers.RepeatedCompositeFieldContainer[OperationDescriptor]
    tracked_properties: _containers.RepeatedScalarFieldContainer[str]
    capabilities: Capabilities
    def __init__(self, id: _Optional[str] = ..., version: _Optional[str] = ..., host: _Optional[_Union[Host, str]] = ..., display_name: _Optional[str] = ..., connection_schema: _Optional[_Union[_struct_pb2.Struct, _Mapping]] = ..., operations: _Optional[_Iterable[_Union[OperationDescriptor, _Mapping]]] = ..., tracked_properties: _Optional[_Iterable[str]] = ..., capabilities: _Optional[_Union[Capabilities, _Mapping]] = ...) -> None: ...

class OperationDescriptor(_message.Message):
    __slots__ = ("type", "custom_type", "supports_browse", "options_schema")
    TYPE_FIELD_NUMBER: _ClassVar[int]
    CUSTOM_TYPE_FIELD_NUMBER: _ClassVar[int]
    SUPPORTS_BROWSE_FIELD_NUMBER: _ClassVar[int]
    OPTIONS_SCHEMA_FIELD_NUMBER: _ClassVar[int]
    type: OperationType
    custom_type: str
    supports_browse: bool
    options_schema: _struct_pb2.Struct
    def __init__(self, type: _Optional[_Union[OperationType, str]] = ..., custom_type: _Optional[str] = ..., supports_browse: _Optional[bool] = ..., options_schema: _Optional[_Union[_struct_pb2.Struct, _Mapping]] = ...) -> None: ...

class Capabilities(_message.Message):
    __slots__ = ("streaming", "max_batch_size", "transactional")
    STREAMING_FIELD_NUMBER: _ClassVar[int]
    MAX_BATCH_SIZE_FIELD_NUMBER: _ClassVar[int]
    TRANSACTIONAL_FIELD_NUMBER: _ClassVar[int]
    streaming: bool
    max_batch_size: int
    transactional: bool
    def __init__(self, streaming: _Optional[bool] = ..., max_batch_size: _Optional[int] = ..., transactional: _Optional[bool] = ...) -> None: ...

class BrowseRequest(_message.Message):
    __slots__ = ("connection", "operation_type", "custom_type", "object_type_id")
    CONNECTION_FIELD_NUMBER: _ClassVar[int]
    OPERATION_TYPE_FIELD_NUMBER: _ClassVar[int]
    CUSTOM_TYPE_FIELD_NUMBER: _ClassVar[int]
    OBJECT_TYPE_ID_FIELD_NUMBER: _ClassVar[int]
    connection: ConnectionConfig
    operation_type: OperationType
    custom_type: str
    object_type_id: str
    def __init__(self, connection: _Optional[_Union[ConnectionConfig, _Mapping]] = ..., operation_type: _Optional[_Union[OperationType, str]] = ..., custom_type: _Optional[str] = ..., object_type_id: _Optional[str] = ...) -> None: ...

class BrowseResponse(_message.Message):
    __slots__ = ("object_types", "request_profile", "response_profile")
    OBJECT_TYPES_FIELD_NUMBER: _ClassVar[int]
    REQUEST_PROFILE_FIELD_NUMBER: _ClassVar[int]
    RESPONSE_PROFILE_FIELD_NUMBER: _ClassVar[int]
    object_types: _containers.RepeatedCompositeFieldContainer[ObjectType]
    request_profile: Profile
    response_profile: Profile
    def __init__(self, object_types: _Optional[_Iterable[_Union[ObjectType, _Mapping]]] = ..., request_profile: _Optional[_Union[Profile, _Mapping]] = ..., response_profile: _Optional[_Union[Profile, _Mapping]] = ...) -> None: ...

class ObjectType(_message.Message):
    __slots__ = ("id", "label")
    ID_FIELD_NUMBER: _ClassVar[int]
    LABEL_FIELD_NUMBER: _ClassVar[int]
    id: str
    label: str
    def __init__(self, id: _Optional[str] = ..., label: _Optional[str] = ...) -> None: ...

class Profile(_message.Message):
    __slots__ = ("format", "definition")
    class Format(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
        __slots__ = ()
        FORMAT_UNSPECIFIED: _ClassVar[Profile.Format]
        FORMAT_JSON_SCHEMA: _ClassVar[Profile.Format]
        FORMAT_XSD: _ClassVar[Profile.Format]
    FORMAT_UNSPECIFIED: Profile.Format
    FORMAT_JSON_SCHEMA: Profile.Format
    FORMAT_XSD: Profile.Format
    FORMAT_FIELD_NUMBER: _ClassVar[int]
    DEFINITION_FIELD_NUMBER: _ClassVar[int]
    format: Profile.Format
    definition: bytes
    def __init__(self, format: _Optional[_Union[Profile.Format, str]] = ..., definition: _Optional[bytes] = ...) -> None: ...

class ConnectionConfig(_message.Message):
    __slots__ = ("settings",)
    SETTINGS_FIELD_NUMBER: _ClassVar[int]
    settings: _struct_pb2.Struct
    def __init__(self, settings: _Optional[_Union[_struct_pb2.Struct, _Mapping]] = ...) -> None: ...

class ExecuteRequest(_message.Message):
    __slots__ = ("start", "chunk", "end")
    START_FIELD_NUMBER: _ClassVar[int]
    CHUNK_FIELD_NUMBER: _ClassVar[int]
    END_FIELD_NUMBER: _ClassVar[int]
    start: ExecuteStart
    chunk: DocumentChunk
    end: ExecuteEnd
    def __init__(self, start: _Optional[_Union[ExecuteStart, _Mapping]] = ..., chunk: _Optional[_Union[DocumentChunk, _Mapping]] = ..., end: _Optional[_Union[ExecuteEnd, _Mapping]] = ...) -> None: ...

class ExecuteStart(_message.Message):
    __slots__ = ("execution_id", "step_id", "connection", "operation_type", "custom_type", "object_type_id", "options")
    EXECUTION_ID_FIELD_NUMBER: _ClassVar[int]
    STEP_ID_FIELD_NUMBER: _ClassVar[int]
    CONNECTION_FIELD_NUMBER: _ClassVar[int]
    OPERATION_TYPE_FIELD_NUMBER: _ClassVar[int]
    CUSTOM_TYPE_FIELD_NUMBER: _ClassVar[int]
    OBJECT_TYPE_ID_FIELD_NUMBER: _ClassVar[int]
    OPTIONS_FIELD_NUMBER: _ClassVar[int]
    execution_id: str
    step_id: str
    connection: ConnectionConfig
    operation_type: OperationType
    custom_type: str
    object_type_id: str
    options: _struct_pb2.Struct
    def __init__(self, execution_id: _Optional[str] = ..., step_id: _Optional[str] = ..., connection: _Optional[_Union[ConnectionConfig, _Mapping]] = ..., operation_type: _Optional[_Union[OperationType, str]] = ..., custom_type: _Optional[str] = ..., object_type_id: _Optional[str] = ..., options: _Optional[_Union[_struct_pb2.Struct, _Mapping]] = ...) -> None: ...

class DocumentChunk(_message.Message):
    __slots__ = ("document_id", "properties", "data", "last")
    class PropertiesEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    DOCUMENT_ID_FIELD_NUMBER: _ClassVar[int]
    PROPERTIES_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    LAST_FIELD_NUMBER: _ClassVar[int]
    document_id: str
    properties: _containers.ScalarMap[str, str]
    data: bytes
    last: bool
    def __init__(self, document_id: _Optional[str] = ..., properties: _Optional[_Mapping[str, str]] = ..., data: _Optional[bytes] = ..., last: _Optional[bool] = ...) -> None: ...

class ExecuteEnd(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ExecuteResponse(_message.Message):
    __slots__ = ("result", "output")
    RESULT_FIELD_NUMBER: _ClassVar[int]
    OUTPUT_FIELD_NUMBER: _ClassVar[int]
    result: DocumentResult
    output: DocumentChunk
    def __init__(self, result: _Optional[_Union[DocumentResult, _Mapping]] = ..., output: _Optional[_Union[DocumentChunk, _Mapping]] = ...) -> None: ...

class DocumentResult(_message.Message):
    __slots__ = ("document_id", "status", "message", "tracked", "output_document_ids")
    class TrackedEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    DOCUMENT_ID_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    TRACKED_FIELD_NUMBER: _ClassVar[int]
    OUTPUT_DOCUMENT_IDS_FIELD_NUMBER: _ClassVar[int]
    document_id: str
    status: DocumentStatus
    message: str
    tracked: _containers.ScalarMap[str, str]
    output_document_ids: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, document_id: _Optional[str] = ..., status: _Optional[_Union[DocumentStatus, str]] = ..., message: _Optional[str] = ..., tracked: _Optional[_Mapping[str, str]] = ..., output_document_ids: _Optional[_Iterable[str]] = ...) -> None: ...

class TestRequest(_message.Message):
    __slots__ = ("connection",)
    CONNECTION_FIELD_NUMBER: _ClassVar[int]
    connection: ConnectionConfig
    def __init__(self, connection: _Optional[_Union[ConnectionConfig, _Mapping]] = ...) -> None: ...

class TestResponse(_message.Message):
    __slots__ = ("ok", "diagnostic")
    OK_FIELD_NUMBER: _ClassVar[int]
    DIAGNOSTIC_FIELD_NUMBER: _ClassVar[int]
    ok: bool
    diagnostic: str
    def __init__(self, ok: _Optional[bool] = ..., diagnostic: _Optional[str] = ...) -> None: ...

class ListenRequest(_message.Message):
    __slots__ = ("start", "ack")
    START_FIELD_NUMBER: _ClassVar[int]
    ACK_FIELD_NUMBER: _ClassVar[int]
    start: ListenStart
    ack: Ack
    def __init__(self, start: _Optional[_Union[ListenStart, _Mapping]] = ..., ack: _Optional[_Union[Ack, _Mapping]] = ...) -> None: ...

class ListenStart(_message.Message):
    __slots__ = ("listener_id", "connection", "options", "max_in_flight")
    LISTENER_ID_FIELD_NUMBER: _ClassVar[int]
    CONNECTION_FIELD_NUMBER: _ClassVar[int]
    OPTIONS_FIELD_NUMBER: _ClassVar[int]
    MAX_IN_FLIGHT_FIELD_NUMBER: _ClassVar[int]
    listener_id: str
    connection: ConnectionConfig
    options: _struct_pb2.Struct
    max_in_flight: int
    def __init__(self, listener_id: _Optional[str] = ..., connection: _Optional[_Union[ConnectionConfig, _Mapping]] = ..., options: _Optional[_Union[_struct_pb2.Struct, _Mapping]] = ..., max_in_flight: _Optional[int] = ...) -> None: ...

class Ack(_message.Message):
    __slots__ = ("document_id", "status")
    DOCUMENT_ID_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    document_id: str
    status: DocumentStatus
    def __init__(self, document_id: _Optional[str] = ..., status: _Optional[_Union[DocumentStatus, str]] = ...) -> None: ...

class ListenResponse(_message.Message):
    __slots__ = ("chunk",)
    CHUNK_FIELD_NUMBER: _ClassVar[int]
    chunk: DocumentChunk
    def __init__(self, chunk: _Optional[_Union[DocumentChunk, _Mapping]] = ...) -> None: ...
