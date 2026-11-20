const $ = id => document.getElementById(id);
const screen = $("stage"), nodes = $("nodes"), svgNS = "http://www.w3.org/2000/svg";
const getLocal = key => { try { return localStorage.getItem(key) || ""; } catch { return ""; } };
const setLocal = (key, val) => { try { localStorage.setItem(key, val); } catch {} };

const wide = matchMedia("(min-width: 921px)");
function colors(el, cat) {
  const hex = value => /^#[a-f\d]{6}$/i.test(value || "");
  el.style.setProperty("--cat", hex(cat?.face_bg) ? cat.face_bg : "#f3f4f6");
  el.style.setProperty("--cat-accent", hex(cat?.accent_color) ? cat.accent_color : "#60a5fa");
}
const isLive = d => !!(d?.ws_connected ?? d?.ws);
const persisted = d => /^virt-[0-9a-f]{8}$/.test(d?.device_id || "");
const node = (tag, cls, text) => {
  const element = document.createElement(tag);
  if (cls) element.className = cls;
  if (text != null) element.textContent = text;
  return element;
};
const say = (id, text) => {
  const element = $(id);
  if (element.textContent !== text) element.textContent = text;
};

export {$, colors, getLocal, isLive, node, nodes, persisted, say, screen, setLocal, svgNS, wide};
