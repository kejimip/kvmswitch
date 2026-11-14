let queue=[], staging=false, stageLabel='', recordsKey='';
const fsize=n=>{let v=n,u=0;while(v>=1024 && u<4){v/=1024;u++}return (u?v.toFixed(1):v)+' '+['B','KiB','MiB','GiB','TiB'][u]};
function el(tag,text){const e=document.createElement(tag);if(text!=null)e.textContent=text;return e}
function fileButton(label,fn){const b=el('button',label);b.className='link';b.onclick=async()=>{try{await fn()}catch(e){toast(e.message,true)}};return b}
function queueView(){
 const div=$('#ftQueue');div.replaceChildren();
 queue.forEach((f,i)=>{const row=el('div');row.className='ft-item';row.append(el('span',f.name+' · '+fsize(f.size)));if(!staging)row.append(fileButton('移除',()=>{queue.splice(i,1);queueView()}));div.append(row)});
 if(state)fileControls(state)
}
function fileControls(s){
 const p=s.state,ready=p.connected&&p.peer_files,busy=(s.files||[]).some(j=>j.Direction==='发送'&&j.Active);
 $('#ftPeer').textContent=!p.connected?'对端离线 · 可先选择文件':!ready?'请更新对端以启用手动文件传输':'已配对 · '+p.peer_name;
 $('#ftSum').textContent=staging?stageLabel:queue.length?'共 '+queue.length+' 个 · '+fsize(queue.reduce((n,f)=>n+f.size,0)):'未选择文件';
 $('#ftPick').disabled=staging;$('#ftSend').disabled=!ready||busy||staging||!queue.length;$('#ftSend').textContent=ready?'发送到 '+p.peer_name:'发送文件';
}
function choose(fs){if(staging)return;for(const f of fs){if(!queue.includes(f))queue.push(f)}queueView()}
$('#ftPick').onclick=()=>$('#ftInput').click();$('#ftInput').onchange=e=>{choose(e.target.files);e.target.value=''};
const dz=$('#ftDrop');window.addEventListener('dragover',e=>{if(e.dataTransfer.types.includes('Files'))e.preventDefault()});
window.addEventListener('drop',e=>{if(e.dataTransfer.types.includes('Files'))e.preventDefault()});
dz.ondragover=e=>{e.preventDefault();dz.classList.add('over')};dz.ondragleave=()=>dz.classList.remove('over');
dz.ondrop=e=>{e.preventDefault();dz.classList.remove('over');if(Array.from(e.dataTransfer.items||[]).some(item=>item.webkitGetAsEntry?.()?.isDirectory)){toast('首版只接收普通文件，请先将文件夹压缩。',true);return}choose(e.dataTransfer.files)};
$('#ftSend').onclick=async()=>{
 if(staging||!queue.length)return;staging=true;stageLabel='正在准备本机文件…';queueView();
 try{const fd=new FormData();fd.append('manifest',JSON.stringify(queue.map(f=>({Name:f.name,Size:f.size}))));for(const f of queue)fd.append('file',f,'file');
 await new Promise((ok,no)=>{const x=new XMLHttpRequest();x.open('POST','/api/files/send');x.setRequestHeader('X-KVM-Token',token);x.setRequestHeader('X-KVM-Session',state.state.file_session);
 x.upload.onprogress=e=>{stageLabel='正在暂存到本机 · '+fsize(e.loaded);$('#ftSum').textContent=stageLabel};x.onload=()=>{if(x.status===202)ok();else{let msg;try{msg=JSON.parse(x.responseText).error}catch{}no(new Error(msg||'提交失败'))}};x.onerror=()=>no(new Error('无法连接本机面板'));x.send(fd)});
 queue=[];toast('文件已交给传输任务');await tick()
 }catch(e){toast(e.message,true)}finally{staging=false;queueView()}
};
const fAction=async(j,Action)=>{await api('/api/files/action',{ID:j.ID,Action});await tick()};
const fFolder=(Action,ID='')=>api('/api/files/folder',{Action,ID});
$('#ftChange').onclick=async()=>{try{const {dir}=await fFolder('pick');if(dir)await save({file_dir:dir})}catch(e){toast(e.message,true)}};
$('#ftOpen').onclick=async()=>{try{await fFolder('open')}catch(e){toast(e.message,true)}};
function fileRecord(j){
 const row=el('div');row.className='ft-record';const head=el('div');head.className='ft-item';head.append(el('b',j.Direction+' · '+j.Name),el('span',j.Status));row.append(head);
 const speed=j.Bytes/Math.max(1,(Date.now()-j.Started)/1000);
 row.append(el('small',new Date(j.Started).toLocaleTimeString()+' · '+j.Peer+' · 已完成 '+j.Completed+'/'+j.Count+' · '+fsize(j.Bytes)+' / '+fsize(j.Total)+(j.Active?' · '+fsize(Math.round(speed))+'/s':'')));
 if(j.Active){const bar=el('progress');bar.max=j.Total||1;bar.value=j.Bytes;row.append(bar,fileButton('取消',()=>fAction(j,'cancel')))}
 if(j.Retry)row.append(fileButton('重发未完成文件',()=>fAction(j,'retry')));
 if(j.Dir&&j.Completed)row.append(fileButton('打开接收目录',()=>fFolder('open',j.ID)));
 return row
}
function drawTransfers(s){
 fileControls(s);$('#ftDir').textContent=s.config.file_dir;
 const rs=s.files||[],key=JSON.stringify(rs);if(key===recordsKey)return;recordsKey=key;
 const now=$('#ftNow'),hist=$('#ftHistory');now.replaceChildren();hist.replaceChildren();
 for(const j of rs)hist.append(fileRecord(j));if(!rs.length)hist.append(el('div','暂无传输记录'));
 const live=rs.filter(j=>j.Active);for(const j of live.length?live:rs.slice(0,1))now.append(fileRecord(j))
}
