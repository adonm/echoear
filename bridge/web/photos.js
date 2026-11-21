import {request} from "./api.js";

const activePhotos = new Map();
const photoHref = /^photo:(photo-[0-9a-f]{8}|lib-[0-9a-f]{32})$/;
const portraitHref = /^portrait:([0-9a-f]{32}):(idle|blink|talk|think)$/;

function retainPhotos(ids) {
  for (const [id, entry] of activePhotos) {
    if (ids.has(id)) continue;
    entry.request.abort();
    if (entry.url) URL.revokeObjectURL(entry.url);
    activePhotos.delete(id);
  }
}

function clearPhotos() {
  retainPhotos(new Set());
}

function stagePhotos(svg, token) {
  const ids = new Set();
  for (const image of svg.querySelectorAll("image")) {
    const match = photoHref.exec(image.getAttribute("href") || "");
    const portrait = portraitHref.exec(image.getAttribute("href") || "");
    image.removeAttribute("href");
    if (!match && !portrait) continue;
    const id = portrait ? portrait[0] : match[1];
    const library = !portrait && id.startsWith("lib-");
    if (portrait) {
      for (const pose of ["idle", "blink", "talk", "think"]) ids.add(`portrait:${portrait[1]}:${pose}`);
    } else ids.add(id);
    let entry = activePhotos.get(id);
    if (!entry) {
      entry = {request:new AbortController()};
      activePhotos.set(id, entry);
      const url = portrait ? `/api/portraits/${portrait[1]}/${portrait[2]}.png` :
        library ? `/api/library/thumbnail/${id}` : `/api/photos/${id}`;
      entry.promise = request(token, url, {signal:entry.request.signal})
        .then(res => res.ok ? res.blob() : null)
        .then(blob => {
          if (blob?.type === (library ? "image/jpeg" : "image/png") && !entry.request.signal.aborted) {
            entry.url = URL.createObjectURL(blob);
          }
        }).catch(() => {});
    }
    if (entry.url) image.setAttribute("href", entry.url);
    else entry.promise.then(() => {
      if (entry.url && !entry.request.signal.aborted && image.isConnected) image.setAttribute("href", entry.url);
    });
  }
  retainPhotos(ids);
}

export {clearPhotos, stagePhotos};
