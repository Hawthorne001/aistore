"""Unit tests for the aistore.sdk.obj.object.Object helper class.

This suite validates Object behavior including reader/writer operations, URL
building, custom metadata handling, and promotion/blob-download helpers. It is
intended to exercise the public API surface thoroughly using mocks only—no
network traffic or AIS cluster is required.
"""

import unittest
from unittest.mock import Mock, patch, mock_open
from json import dumps as json_dumps

import warnings

from requests import Response
from requests.structures import CaseInsensitiveDict
from aistore.sdk.provider import Provider
from aistore.sdk.blob_download_config import BlobDownloadConfig
from aistore.sdk.const import (
    HTTP_METHOD_HEAD,
    DEFAULT_CHUNK_SIZE,
    HTTP_METHOD_PATCH,
    QPARAM_ARCHPATH,
    QPARAM_ARCHREGX,
    QPARAM_ARCHMODE,
    QPARAM_ETL_NAME,
    QPARAM_ETL_ARGS,
    QPARAM_OBJ_APPEND,
    QPARAM_OBJ_APPEND_HANDLE,
    QPARAM_OBJ_TO,
    QPARAM_NEW_CUSTOM,
    HTTP_METHOD_PUT,
    HTTP_METHOD_DELETE,
    HEADER_RANGE,
    HEADER_OBJECT_APPEND_HANDLE,
    HTTP_METHOD_POST,
    ACT_PROMOTE,
    ACT_BLOB_DOWNLOAD,
    URL_PATH_OBJECTS,
    HEADER_OBJECT_BLOB_DOWNLOAD,
    HEADER_OBJECT_BLOB_CHUNK_SIZE,
    HEADER_OBJECT_BLOB_WORKERS,
    AIS_BCK_NAME,
    AIS_BCK_PROVIDER,
    AIS_OBJ_NAME,
    AIS_LOCATION,
    AIS_MIRROR_PATHS,
    AIS_MIRROR_COPIES,
    AIS_PRESENT,
    QPARAM_LATEST,
    QPARAM_SYNC,
)
from aistore.sdk.obj.object import (
    Object,
    BucketDetails,
)  # pylint: disable=protected-access
from aistore.sdk.obj.object_client import ObjectClient
from aistore.sdk.obj.object_reader import ObjectReader
from aistore.sdk.archive_config import ArchiveMode, ArchiveConfig
from aistore.sdk.etl import ETLConfig
from aistore.sdk.obj.object_props import ObjectProps
from aistore.sdk.types import (
    ActionMsg,
    BlobMsg,
    PromoteAPIArgs,
    BucketEntry,
)
from tests.const import SMALL_FILE_SIZE, ETL_NAME
from tests.utils import cases

BCK_NAME = "bucket_name"
OBJ_NAME = "object_name"
DEST_BCK_NAME = "dest-bucket"
REQUEST_PATH = f"{URL_PATH_OBJECTS}/{BCK_NAME}/{OBJ_NAME}"


# pylint: disable=unused-variable, too-many-locals, too-many-public-methods, no-value-for-parameter
class TestObject(unittest.TestCase):
    """Comprehensive unit tests for ``aistore.sdk.obj.object.Object``."""

    def setUp(self) -> None:
        self.mock_client = Mock()
        self.bck_qparams = {"propkey": "propval"}
        self.bucket_details = BucketDetails(
            BCK_NAME, "ais", self.bck_qparams, f"ais/@#/{BCK_NAME}/"
        )
        self.mock_writer = Mock()
        self.expected_params = self.bck_qparams
        self.object = Object(self.mock_client, self.bucket_details, OBJ_NAME)

    def test_properties(self):
        self.assertEqual(BCK_NAME, self.object.bucket_name)
        self.assertEqual("ais", self.object.bucket_provider)
        self.assertEqual(self.bck_qparams, self.object.query_params)
        self.assertEqual(OBJ_NAME, self.object.name)
        self.assertIsNone(self.object.props_cached)
        self.assertIsInstance(self.object.props, ObjectProps)

    def test_head(self):
        self.object.head()

        self.mock_client.request.assert_called_with(
            HTTP_METHOD_HEAD,
            path=REQUEST_PATH,
            params=self.expected_params,
        )

    def test_get_default_params(self):
        self.get_exec_assert()

    @cases(
        {"blob_config": BlobDownloadConfig(chunk_size="4mb", num_workers="10")},
        {"byte_range": "bytes=100-200", "byte_range_tuple": (100, 200)},
        {"byte_range": "bytes=500-", "byte_range_tuple": (500, None)},
        {"byte_range": "bytes=-300", "byte_range_tuple": (None, 300)},
    )
    def test_get(self, case):
        archpath_param = "archpath"
        self.expected_params[QPARAM_ARCHPATH] = archpath_param
        self.expected_params[QPARAM_ARCHREGX] = ""
        self.expected_params[QPARAM_ARCHMODE] = None
        self.expected_params[QPARAM_ETL_NAME] = ETL_NAME
        self.expected_params[QPARAM_ETL_ARGS] = '{"key":"value"}'

        archive_config = ArchiveConfig(archpath=archpath_param)

        blob_config = case.get("blob_config", None)
        byte_range = case.get("byte_range", None)
        byte_range_tuple = case.get("byte_range_tuple", (None, None))

        expected_headers = self.get_expected_headers({}, blob_config, byte_range)

        self.get_exec_assert(
            archive_config=archive_config,
            chunk_size=3,
            etl=ETLConfig(ETL_NAME, {"key": "value"}),
            writer=self.mock_writer,
            blob_download_config=blob_config,
            byte_range=byte_range,
            expected_byte_range_tuple=byte_range_tuple,
            expected_headers=expected_headers,
        )

    def test_get_archregex(self):
        regex = "regex"
        mode = ArchiveMode.PREFIX
        self.expected_params[QPARAM_ARCHPATH] = ""
        self.expected_params[QPARAM_ARCHREGX] = regex
        self.expected_params[QPARAM_ARCHMODE] = mode.value
        archive_config = ArchiveConfig(regex=regex, mode=mode)
        self.get_exec_assert(archive_config=archive_config)

    def test_get_direct(self):
        self.get_exec_assert(
            direct=True, expected_uname=f"{self.bucket_details.path}{OBJ_NAME}"
        )

    @patch("aistore.sdk.obj.object.ObjectReader")
    @patch("aistore.sdk.obj.object.ObjectClient")
    def get_exec_assert(self, mock_obj_client, mock_obj_reader, **kwargs):
        mock_obj_client_instance = Mock(spec=ObjectClient)
        mock_obj_client.return_value = mock_obj_client_instance
        mock_obj_reader.return_value = Mock(spec=ObjectReader)

        expected_headers = kwargs.pop("expected_headers", {})
        expected_byte_range_tuple = kwargs.pop(
            "expected_byte_range_tuple", (None, None)
        )
        expected_chunk_size = kwargs.get("chunk_size", DEFAULT_CHUNK_SIZE)
        expected_uname = kwargs.pop("expected_uname", None)

        res = self.object.get_reader(**kwargs)

        self.assertIsInstance(res, ObjectReader)

        mock_obj_client.assert_called_with(
            request_client=self.mock_client,
            path=REQUEST_PATH,
            params=self.expected_params,
            headers=expected_headers,
            byte_range=expected_byte_range_tuple,
            uname=expected_uname,
        )

        mock_obj_reader.assert_called_with(
            object_client=mock_obj_client_instance,
            chunk_size=expected_chunk_size,
        )
        if "writer" in kwargs:
            self.mock_writer.writelines.assert_called_with(res)

    @staticmethod
    def get_expected_headers(initial_headers, blob_config=None, byte_range=None):
        expected_headers = initial_headers.copy()
        if blob_config:
            blob_chunk_size = blob_config.chunk_size
            blob_workers = blob_config.num_workers
            if blob_chunk_size or blob_workers:
                expected_headers[HEADER_OBJECT_BLOB_DOWNLOAD] = "true"
            if blob_chunk_size:
                expected_headers[HEADER_OBJECT_BLOB_CHUNK_SIZE] = blob_chunk_size
            if blob_workers:
                expected_headers[HEADER_OBJECT_BLOB_WORKERS] = blob_workers
        if byte_range:
            expected_headers[HEADER_RANGE] = byte_range

        return expected_headers

    def test_get_url(self):
        expected_res = "full url"
        archpath = "arch"
        self.mock_client.get_full_url.return_value = expected_res
        self.expected_params[QPARAM_ARCHPATH] = archpath
        self.expected_params[QPARAM_ETL_NAME] = ETL_NAME

        res = self.object.get_url(archpath=archpath, etl=ETLConfig(ETL_NAME))

        self.assertEqual(expected_res, res)
        self.mock_client.get_full_url.assert_called_with(
            REQUEST_PATH, self.expected_params
        )

    @patch("pathlib.Path.is_file")
    @patch("pathlib.Path.exists")
    def test_put_file(self, mock_exists, mock_is_file):
        mock_exists.return_value = True
        mock_is_file.return_value = True
        path = "any/filepath"
        mock_file = mock_open(read_data=b"file content")
        with patch("builtins.open", mock_file):
            self.object.get_writer().put_file(path)

        self.mock_client.request.assert_called_with(
            HTTP_METHOD_PUT,
            path=REQUEST_PATH,
            params=self.expected_params,
            data=mock_file.return_value,
        )

    def test_put_content(self):
        content = b"user-supplied-bytes"
        self.object.get_writer().put_content(content)
        self.mock_client.request.assert_called_with(
            HTTP_METHOD_PUT,
            path=REQUEST_PATH,
            params=self.expected_params,
            data=content,
        )

    def test_append_content(self):
        content = b"content-to-append"
        expected_handle = "TEST_HANDLE"
        self.expected_params[QPARAM_OBJ_APPEND] = "append"
        self.expected_params[QPARAM_OBJ_APPEND_HANDLE] = ""
        resp_headers = CaseInsensitiveDict(
            {HEADER_OBJECT_APPEND_HANDLE: expected_handle}
        )
        mock_response = Mock(Response)
        mock_response.headers = resp_headers
        self.mock_client.request.return_value = mock_response

        next_handle = self.object.get_writer().append_content(content)
        self.mock_client.request.assert_called_once_with(
            HTTP_METHOD_PUT,
            path=REQUEST_PATH,
            params=self.expected_params,
            data=content,
        )
        self.assertEqual(next_handle, expected_handle)

    def test_append_flush(self):
        expected_handle = ""
        prev_handle = "prev_handle"
        self.expected_params[QPARAM_OBJ_APPEND] = "flush"
        self.expected_params[QPARAM_OBJ_APPEND_HANDLE] = prev_handle
        resp_headers = CaseInsensitiveDict({})
        mock_response = Mock(Response)
        mock_response.headers = resp_headers
        self.mock_client.request.return_value = mock_response

        next_handle = self.object.get_writer().append_content(b"", prev_handle, True)
        self.mock_client.request.assert_called_once_with(
            HTTP_METHOD_PUT,
            path=REQUEST_PATH,
            params=self.expected_params,
            data=b"",
        )
        self.assertEqual(next_handle, expected_handle)

    def test_set_custom_props(self):
        custom_metadata = {"key1": "value1", "key2": "value2"}
        expected_json_val = ActionMsg(action="", value=custom_metadata).model_dump()

        self.object.get_writer().set_custom_props(custom_metadata)

        self.mock_client.request.assert_called_with(
            HTTP_METHOD_PATCH,
            path=REQUEST_PATH,
            params=self.expected_params,
            json=expected_json_val,
        )

    def test_set_custom_props_with_replace_existing(self):
        custom_metadata = {"key1": "value1", "key2": "value2"}
        self.expected_params[QPARAM_NEW_CUSTOM] = "true"
        expected_json_val = ActionMsg(action="", value=custom_metadata).model_dump()

        self.object.get_writer().set_custom_props(
            custom_metadata, replace_existing=True
        )

        self.mock_client.request.assert_called_with(
            HTTP_METHOD_PATCH,
            path=REQUEST_PATH,
            params=self.expected_params,
            json=expected_json_val,
        )

    def test_promote_default_args(self):
        filename = "promoted file"
        expected_value = PromoteAPIArgs(source_path=filename, object_name=OBJ_NAME)
        self.promote_exec_assert(filename, expected_value)

    def test_promote(self):
        filename = "promoted file"
        target_id = "target node"
        recursive = True
        overwrite_dest = True
        delete_source = True
        src_not_file_share = True
        expected_value = PromoteAPIArgs(
            source_path=filename,
            object_name=OBJ_NAME,
            target_id=target_id,
            recursive=recursive,
            overwrite_dest=overwrite_dest,
            delete_source=delete_source,
            src_not_file_share=src_not_file_share,
        )
        self.promote_exec_assert(
            filename,
            expected_value,
            target_id=target_id,
            recursive=recursive,
            overwrite_dest=overwrite_dest,
            delete_source=delete_source,
            src_not_file_share=src_not_file_share,
        )

    def promote_exec_assert(self, filename, expected_value, **kwargs):
        request_path = f"{URL_PATH_OBJECTS}/{BCK_NAME}"
        expected_json = ActionMsg(
            action=ACT_PROMOTE, name=filename, value=expected_value.as_dict()
        ).model_dump()
        self.object.promote(filename, **kwargs)
        self.mock_client.request.assert_called_with(
            HTTP_METHOD_POST,
            path=request_path,
            params=self.expected_params,
            json=expected_json,
        )

    def test_delete(self):
        self.object.delete()
        self.mock_client.request.assert_called_with(
            HTTP_METHOD_DELETE, path=REQUEST_PATH, params=self.expected_params
        )

    def test_blob_download_default_args(self):
        request_path = f"{URL_PATH_OBJECTS}/{BCK_NAME}"
        expected_blob_msg = BlobMsg(
            chunk_size=None,
            num_workers=None,
            latest=False,
        ).as_dict()
        expected_json = ActionMsg(
            action=ACT_BLOB_DOWNLOAD, name=OBJ_NAME, value=expected_blob_msg
        ).model_dump()
        self.object.blob_download()
        self.mock_client.request.assert_called_with(
            HTTP_METHOD_POST,
            path=request_path,
            params=self.expected_params,
            json=expected_json,
        )

    def test_blob_download(self):
        request_path = f"{URL_PATH_OBJECTS}/{BCK_NAME}"
        chunk_size = SMALL_FILE_SIZE
        num_workers = 10
        latest = True
        expected_blob_msg = BlobMsg(
            chunk_size=chunk_size,
            num_workers=num_workers,
            latest=latest,
        ).as_dict()
        expected_json = ActionMsg(
            action=ACT_BLOB_DOWNLOAD, name=OBJ_NAME, value=expected_blob_msg
        ).model_dump()
        self.object.blob_download(
            num_workers=num_workers, chunk_size=chunk_size, latest=latest
        )
        self.mock_client.request.assert_called_with(
            HTTP_METHOD_POST,
            path=request_path,
            params=self.expected_params,
            json=expected_json,
        )

    def test_object_props(self):

        headers = CaseInsensitiveDict(
            {
                "Ais-Atime": "1722021816727999173",
                "Ais-Bucket-Name": "data-bck",
                "Ais-Bucket-Provider": "ais",
                "Ais-Checksum-Type": "xxhash",
                "Ais-Checksum-Value": "ecc0a7bf787e089e",
                "Ais-Location": "t[LSJt8081]:mp[/tmp/ais/mp1/1, [sda sdb]]",
                "Ais-Mirror-Copies": "1",
                "Ais-Mirror-Paths": "[/tmp/ais/mp1/1]",
                "Ais-Name": "cifar-10-batches-py/batches.meta",
                "Ais-Present": "true",
                "Ais-Version": "1",
                "Content-Length": "158",
                "Date": "Wed, 31 Jul 2024 16:55:14 GMT",
            }
        )

        self.mock_client.request.return_value = Mock(headers=headers)

        self.assertEqual(self.object.props_cached, None)

        self.object.head()

        props: ObjectProps = self.object.props

        self.assertEqual(props.bucket_name, headers[AIS_BCK_NAME])
        self.assertEqual(props.bucket_provider, headers[AIS_BCK_PROVIDER])
        self.assertEqual(props.name, headers[AIS_OBJ_NAME])
        self.assertEqual(props.location, headers[AIS_LOCATION])
        self.assertEqual(
            props.mirror_paths, headers[AIS_MIRROR_PATHS].strip("[]").split(",")
        )
        self.assertEqual(props.mirror_copies, int(headers[AIS_MIRROR_COPIES]))
        self.assertEqual(props.present, headers[AIS_PRESENT] == "true")

    def test_generate_object_props(self):
        entry = BucketEntry(
            n="NAME", cs="CHECKSUM", a="ATIME", v="VERSION", t="LOCATION", s=5, c=6
        )

        props: ObjectProps = entry.generate_object_props()

        self.assertEqual(props.checksum_value, entry.cs)
        self.assertEqual(props.name, entry.n)
        self.assertEqual(props.location, entry.t)
        self.assertEqual(props.mirror_copies, entry.c)
        self.assertEqual(props.obj_version, entry.v)
        self.assertEqual(props.size, entry.s)
        self.assertEqual(props.access_time, entry.a)

    @patch.object(Object, "get_reader", return_value="READER")
    def test_get_deprecated_wrapper(self, mock_get_reader):
        """Ensure Object.get emits DeprecationWarning and forwards to get_reader."""
        with warnings.catch_warnings(record=True) as w:
            warnings.simplefilter("always", DeprecationWarning)
            result = self.object.get()
            mock_get_reader.assert_called_once()
            self.assertEqual(result, "READER")
            self.assertTrue(
                any(issubclass(item.category, DeprecationWarning) for item in w)
            )

    def test_get_semantic_url(self):
        """Verify get_semantic_url without touching protected members."""

        temp_details = BucketDetails(
            BCK_NAME,
            Provider.AIS,
            self.bck_qparams,
            f"ais/@#/{BCK_NAME}/",
        )
        temp_obj = Object(self.mock_client, temp_details, OBJ_NAME)

        expected = f"{Provider.AIS.value}://{BCK_NAME}/{OBJ_NAME}"
        self.assertEqual(temp_obj.get_semantic_url(), expected)

    @patch.object(Object, "get_writer")
    def test_put_content_deprecated_wrapper(self, mock_get_writer):
        """Ensure put_content forwards to writer and emits DeprecationWarning."""
        mock_writer = Mock()
        mock_writer.put_content.return_value = "RESP"
        mock_get_writer.return_value = mock_writer

        with warnings.catch_warnings(record=True) as w:
            warnings.simplefilter("always", DeprecationWarning)
            resp = self.object.put_content(b"data")

            mock_get_writer.assert_called_once()
            mock_writer.put_content.assert_called_once_with(b"data")
            self.assertEqual(resp, "RESP")
            self.assertTrue(
                any(issubclass(item.category, DeprecationWarning) for item in w)
            )

    @patch.object(Object, "get_writer")
    def test_put_file_deprecated_wrapper(self, mock_get_writer):
        """Ensure put_file forwards to writer and emits DeprecationWarning."""
        mock_writer = Mock()
        mock_writer.put_file.return_value = "RESP"
        mock_get_writer.return_value = mock_writer

        with warnings.catch_warnings(record=True) as w:
            warnings.simplefilter("always", DeprecationWarning)
            resp = self.object.put_file("/tmp/f")

            mock_get_writer.assert_called_once()
            mock_writer.put_file.assert_called_once_with("/tmp/f")
            self.assertEqual(resp, "RESP")
            self.assertTrue(
                any(issubclass(item.category, DeprecationWarning) for item in w)
            )

    @patch.object(Object, "get_writer")
    def test_append_content_deprecated_wrapper(self, mock_get_writer):
        """Ensure append_content forwards to writer and emits DeprecationWarning."""
        mock_writer = Mock()
        mock_writer.append_content.return_value = "HANDLE"
        mock_get_writer.return_value = mock_writer

        with warnings.catch_warnings(record=True) as w:
            warnings.simplefilter("always", DeprecationWarning)
            handle = self.object.append_content(b"data", handle="prev", flush=True)

            mock_get_writer.assert_called_once()
            mock_writer.append_content.assert_called_once_with(b"data", "prev", True)
            self.assertEqual(handle, "HANDLE")
            self.assertTrue(
                any(issubclass(item.category, DeprecationWarning) for item in w)
            )

    @patch.object(Object, "get_writer")
    def test_set_custom_props_deprecated_wrapper(self, mock_get_writer):
        """Ensure set_custom_props forwards to writer and emits DeprecationWarning."""
        mock_writer = Mock()
        mock_writer.set_custom_props.return_value = "RESP"
        mock_get_writer.return_value = mock_writer

        custom = {"a": "b"}

        with warnings.catch_warnings(record=True) as w:
            warnings.simplefilter("always", DeprecationWarning)
            resp = self.object.set_custom_props(custom, replace_existing=True)

            mock_get_writer.assert_called_once()
            mock_writer.set_custom_props.assert_called_once_with(custom, True)
            self.assertEqual(resp, "RESP")
            self.assertTrue(
                any(issubclass(item.category, DeprecationWarning) for item in w)
            )

    def test_get_url_with_etl_args(self):
        """Ensure get_url adds both etl_name and etl_args params when args provided."""
        expected_res = "full url with etl args"
        self.mock_client.get_full_url.return_value = expected_res

        etl_args = {"x": "y"}
        etl_cfg = ETLConfig(ETL_NAME, etl_args)

        expected_params = self.bck_qparams.copy()
        expected_params[QPARAM_ETL_NAME] = ETL_NAME
        expected_params[QPARAM_ETL_ARGS] = etl_args

        res = self.object.get_url(etl=etl_cfg)

        self.assertEqual(res, expected_res)
        self.mock_client.get_full_url.assert_called_with(REQUEST_PATH, expected_params)

    def test_get_reader_byte_range_and_blob_conflict(self):
        """Ensure get_reader raises ValueError when both byte_range and blob_download_config are provided."""
        blob_cfg = BlobDownloadConfig(chunk_size="1mb")
        with self.assertRaises(ValueError):
            self.object.get_reader(
                blob_download_config=blob_cfg, byte_range="bytes=0-100"
            )

    def test_get_reader_latest_param(self):
        """Ensure get_reader sets ?latest=true when latest flag is provided."""
        with patch("aistore.sdk.obj.object.ObjectClient") as mock_client_cls:
            mock_client = Mock()
            mock_client_cls.return_value = mock_client

            with patch("aistore.sdk.obj.object.ObjectReader") as mock_reader_cls:
                mock_reader = Mock()
                mock_reader_cls.return_value = mock_reader

                self.object.get_reader(latest=True)
                args, kwargs = mock_client_cls.call_args
                params_passed = kwargs.get("params", {})
                self.assertIn(QPARAM_LATEST, params_passed)
                self.assertEqual(params_passed[QPARAM_LATEST], "true")

    def test_copy_same_name(self):
        """Test copying object with same name to another bucket."""
        dest_bucket_details = BucketDetails(
            DEST_BCK_NAME, Provider.AIS, {"provider": "ais"}, f"ais/@#/{DEST_BCK_NAME}/"
        )
        dest_object = Object(self.mock_client, dest_bucket_details, OBJ_NAME)

        mock_response = Mock()
        mock_response.status_code = 200
        self.mock_client.request.return_value = mock_response

        response = self.object.copy(dest_object)

        self.assertEqual(response, mock_response)
        expected_params = self.bck_qparams.copy()
        expected_params[QPARAM_OBJ_TO] = f"ais/@#/{DEST_BCK_NAME}/{OBJ_NAME}"
        expected_params[QPARAM_LATEST] = "false"
        expected_params[QPARAM_SYNC] = "false"

        self.mock_client.request.assert_called_once_with(
            HTTP_METHOD_PUT,
            path=REQUEST_PATH,
            params=expected_params,
        )

    def test_copy_different_name(self):
        """Test copying object with different name to another bucket."""
        new_name = "copied-object.txt"
        dest_bucket_details = BucketDetails(
            DEST_BCK_NAME, Provider.AIS, {"provider": "ais"}, f"ais/@#/{DEST_BCK_NAME}/"
        )
        dest_object = Object(self.mock_client, dest_bucket_details, new_name)

        mock_response = Mock()
        mock_response.status_code = 200
        self.mock_client.request.return_value = mock_response

        response = self.object.copy(dest_object)

        self.assertEqual(response, mock_response)
        expected_params = self.bck_qparams.copy()
        expected_params[QPARAM_OBJ_TO] = f"ais/@#/{DEST_BCK_NAME}/{new_name}"
        expected_params[QPARAM_LATEST] = "false"
        expected_params[QPARAM_SYNC] = "false"

        self.mock_client.request.assert_called_once_with(
            HTTP_METHOD_PUT,
            path=REQUEST_PATH,
            params=expected_params,
        )

    def test_copy_with_etl(self):
        """Test copying object with ETL transformation."""
        dest_bucket_details = BucketDetails(
            DEST_BCK_NAME, Provider.AIS, {"provider": "ais"}, f"ais/@#/{DEST_BCK_NAME}/"
        )
        dest_object = Object(self.mock_client, dest_bucket_details, OBJ_NAME)

        mock_response = Mock()
        mock_response.status_code = 200
        self.mock_client.request.return_value = mock_response

        etl_config = ETLConfig(name=ETL_NAME)

        response = self.object.copy(dest_object, etl=etl_config)

        self.assertEqual(response, mock_response)
        expected_params = self.bck_qparams.copy()
        expected_params[QPARAM_OBJ_TO] = f"ais/@#/{DEST_BCK_NAME}/{OBJ_NAME}"
        expected_params[QPARAM_ETL_NAME] = ETL_NAME
        expected_params[QPARAM_LATEST] = "false"
        expected_params[QPARAM_SYNC] = "false"

        self.mock_client.request.assert_called_once_with(
            HTTP_METHOD_PUT,
            path=REQUEST_PATH,
            params=expected_params,
        )

    def test_copy_with_etl_args(self):
        """Test copying object with ETL transformation and etl_args."""
        dest_bucket_details = BucketDetails(
            DEST_BCK_NAME, Provider.AIS, {"provider": "ais"}, f"ais/@#/{DEST_BCK_NAME}/"
        )
        dest_object = Object(self.mock_client, dest_bucket_details, OBJ_NAME)

        mock_response = Mock()
        mock_response.status_code = 200
        self.mock_client.request.return_value = mock_response

        etl_config = ETLConfig(name=ETL_NAME, args={"seed": "42", "mode": "test"})

        response = self.object.copy(dest_object, etl=etl_config)

        self.assertEqual(response, mock_response)
        expected_params = self.bck_qparams.copy()
        expected_params[QPARAM_OBJ_TO] = f"ais/@#/{DEST_BCK_NAME}/{OBJ_NAME}"
        expected_params[QPARAM_ETL_NAME] = ETL_NAME
        expected_params[QPARAM_ETL_ARGS] = json_dumps(
            etl_config.args, separators=(",", ":")
        )
        expected_params[QPARAM_LATEST] = "false"
        expected_params[QPARAM_SYNC] = "false"

        self.mock_client.request.assert_called_once_with(
            HTTP_METHOD_PUT,
            path=REQUEST_PATH,
            params=expected_params,
        )
