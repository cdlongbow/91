import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const drivesPageSource = readFileSync(
  new URL("../src/admin/DrivesPage.tsx", import.meta.url),
  "utf8"
);
const driveFormSource = readFileSync(
  new URL("../src/admin/drive/DriveForm.tsx", import.meta.url),
  "utf8"
);
const constantsSource = readFileSync(
  new URL("../src/admin/drive/constants.ts", import.meta.url),
  "utf8"
);

const combinedSource = drivesPageSource + "\n" + driveFormSource + "\n" + constantsSource + "\n" + readFileSync(
  new URL("../src/admin/drive/Spider91UploadTargetField.tsx", import.meta.url),
  "utf8"
);

function driveTypeOptions() {
  const match = /const DRIVE_OPTIONS:\s*DriveOption\[]\s*=\s*\[([\s\S]*?)\];/.exec(
    driveFormSource
  );
  assert.ok(match, "drive option card list should be present");
  return Array.from(
    match[1].matchAll(/\{\s*kind:\s*"([^"]+)",\s*label:\s*"([^"]+)"/g),
    (option) => ({ value: option[1], label: option[2] })
  );
}

function assertDriveTypeOption(value: string, label: string) {
  assert.ok(
    driveTypeOptions().some((option) => option.value === value && option.label === label),
    `${value} drive type option should be present`
  );
}

test("spider91 drive form does not expose advanced crawler credentials", () => {
  assert.match(combinedSource, /key: "proxy"/);
  assert.match(combinedSource, /label: "代理地址（可选）"/);
  assert.match(combinedSource, /支持 http:\/\/、https:\/\/、socks5:\/\/ 或 socks5h:\/\//);
  assert.doesNotMatch(combinedSource, /target_new/);
  assert.doesNotMatch(combinedSource, /crawl_hour/);
  assert.doesNotMatch(combinedSource, /python_path/);
  assert.doesNotMatch(combinedSource, /script_path/);
});

test("spider91 upload target uses explicit local-save option instead of auto target", () => {
  assert.match(combinedSource, /本地保存，不上传/);
  assert.match(
    combinedSource,
    /d\.kind === "pikpak" \|\| d\.kind === "p115" \|\| d\.kind === "p123" \|\| d\.kind === "onedrive"/
  );
  assert.doesNotMatch(combinedSource, /自动：唯一/);
  assert.doesNotMatch(combinedSource, /自动模式/);
});

test("drive form hides root directory id for localstorage and spider91", () => {
  assert.match(combinedSource, /<label[^>]*>根目录 ID<\/label>/);
  assert.match(
    combinedSource,
    /usesRootDirectoryID\(kind:\s*Kind\):\s*boolean\s*\{\s*return kind !== "localstorage" && kind !== "spider91";\s*\}/
  );
  assert.match(combinedSource, /\{usesRootDirectoryID\(form\.kind\) && \(/);
  assert.match(combinedSource, /\{usesRootDirectoryID\(d\.kind\) && \(/);
  assert.match(combinedSource, /placeholder=\{rootIdPlaceholder\(form\.kind\)\}/);
  assert.doesNotMatch(combinedSource, /扫描起点目录 ID/);
  assert.doesNotMatch(combinedSource, /set\("scanRootId"/);
});

test("onedrive drive form only exposes required default-app fields", () => {
  const match =
    /case "onedrive":\s*return \[([\s\S]*?)\];\s*case "googledrive":/.exec(
      combinedSource
    );
  assert.ok(match, "onedrive credential field block should be present");
  const fields = match[1];

  assert.match(fields, /key: "refresh_token"/);
  assert.doesNotMatch(fields, /key: "access_token"/);
  assert.doesNotMatch(fields, /key: "api_url_address"/);
  assert.doesNotMatch(fields, /key: "region"/);
  assert.doesNotMatch(fields, /key: "is_sharepoint"/);
  assert.doesNotMatch(fields, /key: "site_id"/);
});

test("googledrive drive form only exposes refresh token", () => {
  assertDriveTypeOption("googledrive", "Google Drive");

  const match =
    /case "googledrive":\s*return \[([\s\S]*?)\];\s*case "localstorage":/.exec(
      combinedSource
    );
  assert.ok(match, "googledrive credential field block should be present");
  const fields = match[1];

  assert.match(fields, /key: "refresh_token"/);
  assert.doesNotMatch(fields, /key: "access_token"/);
  assert.doesNotMatch(fields, /key: "api_url_address"/);
  assert.doesNotMatch(fields, /key: "client_id"/);
  assert.doesNotMatch(fields, /key: "client_secret"/);
});

test("pikpak drive form only exposes account login fields", () => {
  const match =
    /case "pikpak":\s*return \[([\s\S]*?)\];\s*case "wopan":/.exec(
      combinedSource
    );
  assert.ok(match, "pikpak credential field block should be present");
  const fields = match[1];

  assert.match(fields, /key: "username"/);
  assert.match(fields, /key: "password"/);
  assert.doesNotMatch(fields, /key: "platform"/);
  assert.doesNotMatch(fields, /key: "refresh_token"/);
  assert.doesNotMatch(fields, /key: "captcha_token"/);
  assert.doesNotMatch(fields, /key: "device_id"/);
  assert.doesNotMatch(fields, /key: "disable_media_link"/);
});

test("localstorage drive form asks for a server directory path", () => {
  assertDriveTypeOption("localstorage", "本地存储");

  const match =
    /case "localstorage":\s*return \[([\s\S]*?)\];\s*case "spider91":/.exec(
      combinedSource
    );
  assert.ok(match, "localstorage credential field block should be present");
  const fields = match[1];

  assert.match(fields, /key: "path"/);
  assert.match(fields, /label: "本地目录路径"/);
  assert.match(combinedSource, /if \(kind === "localstorage"\) return "\/"/);
  assert.match(combinedSource, /kind !== "localstorage" && kind !== "spider91"/);
});

test("drive type selector keeps primary source order", () => {
  assert.deepEqual(driveTypeOptions(), [
    { value: "p115", label: "115 网盘" },
    { value: "p123", label: "123 云盘" },
    { value: "pikpak", label: "PikPak" },
    { value: "onedrive", label: "OneDrive" },
    { value: "googledrive", label: "Google Drive" },
    { value: "localstorage", label: "本地存储" },
    { value: "spider91", label: "91 爬虫" },
    { value: "quark", label: "夸克网盘" },
    { value: "wopan", label: "联通沃盘" },
  ]);
});
