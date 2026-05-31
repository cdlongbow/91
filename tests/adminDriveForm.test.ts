import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const drivesPageSource = readFileSync(
  new URL("../src/admin/DrivesPage.tsx", import.meta.url),
  "utf8"
);

test("spider91 drive form does not expose advanced crawler credentials", () => {
  assert.doesNotMatch(drivesPageSource, /target_new/);
  assert.doesNotMatch(drivesPageSource, /crawl_hour/);
  assert.doesNotMatch(drivesPageSource, /python_path/);
  assert.doesNotMatch(drivesPageSource, /script_path/);
});

test("spider91 upload target uses explicit local-save option instead of auto target", () => {
  assert.match(drivesPageSource, /本地保存，不上传/);
  assert.match(
    drivesPageSource,
    /d\.kind === "pikpak" \|\| d\.kind === "p115" \|\| d\.kind === "onedrive"/
  );
  assert.doesNotMatch(drivesPageSource, /自动：唯一/);
  assert.doesNotMatch(drivesPageSource, /自动模式/);
});

test("drive form shows a root directory id field for all drive kinds", () => {
  assert.match(drivesPageSource, /<label>根目录 ID<\/label>/);
  assert.match(drivesPageSource, /placeholder=\{rootIdPlaceholder\(form\.kind\)\}/);
  assert.doesNotMatch(drivesPageSource, /扫描起点目录 ID/);
  assert.doesNotMatch(drivesPageSource, /set\("scanRootId"/);
});

test("onedrive drive form only exposes required default-app fields", () => {
  const match =
    /function credentialFields[\s\S]*?case "onedrive":\s*return \[([\s\S]*?)\];\s*case "googledrive":/.exec(
      drivesPageSource
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
  assert.match(drivesPageSource, /<option value="googledrive">Google Drive<\/option>/);

  const match =
    /case "googledrive":\s*return \[([\s\S]*?)\];\s*case "localstorage":/.exec(
      drivesPageSource
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
      drivesPageSource
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
  assert.match(drivesPageSource, /<option value="localstorage">本地存储<\/option>/);

  const match =
    /case "localstorage":\s*return \[([\s\S]*?)\];\s*case "spider91":/.exec(
      drivesPageSource
    );
  assert.ok(match, "localstorage credential field block should be present");
  const fields = match[1];

  assert.match(fields, /key: "path"/);
  assert.match(fields, /label: "本地目录路径"/);
  assert.match(drivesPageSource, /if \(kind === "localstorage"\) return "\/"/);
});

test("drive type selector keeps primary source order", () => {
  const options = Array.from(
    drivesPageSource.matchAll(/<option value="([^"]+)">([^<]+)<\/option>/g),
    (match) => ({ value: match[1], label: match[2] })
  );
  const driveOptions = options.slice(0, 8);

  assert.deepEqual(driveOptions, [
    { value: "p115", label: "115 网盘" },
    { value: "pikpak", label: "PikPak" },
    { value: "onedrive", label: "OneDrive" },
    { value: "googledrive", label: "Google Drive" },
    { value: "localstorage", label: "本地存储" },
    { value: "spider91", label: "91 Spider" },
    { value: "quark", label: "夸克网盘" },
    { value: "wopan", label: "联通沃盘" },
  ]);
});
