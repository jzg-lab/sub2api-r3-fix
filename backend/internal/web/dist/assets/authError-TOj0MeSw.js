function a(s){const e=s||{};return e.response?.data?.detail||e.response?.data?.message||e.message||""}function n(s,e){const{fallback:r}=e;return a(s)||r}export{n as b};
